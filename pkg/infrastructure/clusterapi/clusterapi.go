package clusterapi

import (
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	utilkubeconfig "sigs.k8s.io/cluster-api/util/kubeconfig"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"

	"github.com/openshift/installer/pkg/asset"
	"github.com/openshift/installer/pkg/asset/cluster/metadata"
	"github.com/openshift/installer/pkg/asset/ignition/bootstrap"
	"github.com/openshift/installer/pkg/asset/ignition/machine"
	"github.com/openshift/installer/pkg/asset/installconfig"
	"github.com/openshift/installer/pkg/asset/kubeconfig"
	"github.com/openshift/installer/pkg/asset/manifests/capiutils"
	capimanifests "github.com/openshift/installer/pkg/asset/manifests/clusterapi"
	"github.com/openshift/installer/pkg/clusterapi"
	"github.com/openshift/installer/pkg/infrastructure"
	"github.com/openshift/installer/pkg/types"
)

// InfraProvider implements the infrastructure/provider interface
// which is the interface between the rest of the installer and
// the CAPI infrastructure provider.
type InfraProvider struct {
	infrastructure.Provider

	capiProvider Provider
}

// InitializeProvider returns a CAPI provider implementation for a specific
// cloud platform.
func InitializeProvider(platform Provider) infrastructure.Provider {
	return InfraProvider{capiProvider: platform}
}

// Provider provides an interface, intended for platform-specific implementations,
// for calling functions at different points of the CAPI infrastructure provisioning
// lifecycle.
type Provider interface {

	// Ignition generates the ignition secrets for bootstrap and control-plane machines
	// and handles any preconditions for ignition.
	Ignition(in IgnitionInput) ([]client.Object, error)
}

// IgnitionInput collects the args to be passed to the Ignition call.
type IgnitionInput struct {
	MasterIgnData, BootstrapIgnData []byte
	InfraID                         string
	InstallConfig                   *installconfig.InstallConfig
}

// PreProvider can be implemented by cloud platforms to perform functions before
// CAPI provisioning has begun.
type PreProvider interface {
	// PreProvision is called before provisioning using CAPI controllers has begun
	// and should be used to create dependencies needed for CAPI provisioning,
	// such as IAM roles or policies.
	PreProvision(in PreProvisionInput) error
}

// PreProvisionInput collects the args to be passed to the PreProvision call.
type PreProvisionInput struct {
	ClusterID     string
	InstallConfig *installconfig.InstallConfig
}

// InfraReadyProvider can be implemented by cloud platforms to perform functions
// after CAPI has provisioned infrastructure.
type InfraReadyProvider interface {
	// InfraReady is called once cluster.Status.InfrastructureReady
	// is true, typically after load balancers have been provisioned. It can be used
	// to create DNS records.
	InfraReady(in InfraReadyInput) error
}

// InfraReadyInput collects the args to be passed to the InfraReady call.
type InfraReadyInput struct {
	// Client is the client for kube-apiserver running locally on the installer host.
	// It can be used to read the spec and status of the local CAPI resources.
	Client        client.Client
	InstallConfig *installconfig.InstallConfig
}

// Provision creates cluster resources by applying CAPI manifests to a locally running control plane.
func (i InfraProvider) Provision(dir string, parents asset.Parents) ([]*asset.File, error) {
	capiManifestsAsset := &capimanifests.Cluster{}
	clusterKubeconfigAsset := &kubeconfig.AdminClient{}
	clusterID := &installconfig.ClusterID{}
	installConfig := &installconfig.InstallConfig{}
	bootstrapIgnAsset := &bootstrap.Bootstrap{}
	masterIgnAsset := &machine.Master{}
	parents.Get(
		capiManifestsAsset,
		clusterKubeconfigAsset,
		clusterID,
		installConfig,
		bootstrapIgnAsset,
		masterIgnAsset,
	)

	fileList := []*asset.File{}
	manifests := []client.Object{}
	for _, m := range capiManifestsAsset.RuntimeFiles() {
		manifests = append(manifests, m.Object)
	}

	preProvisionInput := PreProvisionInput{
		ClusterID:     clusterID.InfraID,
		InstallConfig: installConfig,
	}

	if p, ok := i.capiProvider.(PreProvider); ok {
		if err := p.PreProvision(preProvisionInput); err != nil {
			return fileList, fmt.Errorf("failed during pre-provisioning: %w", err)
		}
	}

	ignInput := IgnitionInput{
		BootstrapIgnData: bootstrapIgnAsset.Files()[0].Data,
		MasterIgnData:    masterIgnAsset.Files()[0].Data,
		InfraID:          clusterID.InfraID,
		InstallConfig:    installConfig,
	}
	ignSecrets, err := i.capiProvider.Ignition(ignInput)
	if err != nil {
		return fileList, fmt.Errorf("failed during ignition secret generation: %w", err)
	}
	manifests = append(manifests, ignSecrets...)

	// TODO(vincepri): The context should be passed down from the caller,
	// although today the Asset interface doesn't allow it, refactor once it does.
	ctx, cancel := context.WithCancel(signals.SetupSignalHandler())
	defer cancel()
	go func() {
		<-ctx.Done()
		cancel()
		clusterapi.System().Teardown()
	}()
	// Run the CAPI system.
	capiSystem := clusterapi.System()
	if err := capiSystem.Run(ctx, installConfig); err != nil {
		return fileList, fmt.Errorf("failed to run cluster api system: %w", err)
	}

	// Grab the client.
	cl := capiSystem.Client()

	// Create all the manifests and store them.
	for _, m := range manifests {
		m.SetNamespace(capiutils.Namespace)
		if err := cl.Create(context.Background(), m); err != nil {
			return fileList, fmt.Errorf("failed to create manifest: %w", err)
		}
		logrus.Infof("Created manifest %+T, namespace=%s name=%s", m, m.GetNamespace(), m.GetName())
	}

	// Pass cluster kubeconfig and store it in; this is usually the role of a bootstrap provider.
	{
		key := client.ObjectKey{
			Name:      clusterID.InfraID,
			Namespace: capiutils.Namespace,
		}
		cluster := &clusterv1.Cluster{}
		if err := cl.Get(context.Background(), key, cluster); err != nil {
			// TODO (padillon): from this point forward statuses could be
			// collected from the manifests appended to the fileList.
			return fileList, err
		}
		// Create the secret.
		clusterKubeconfig := clusterKubeconfigAsset.Files()[0].Data
		secret := utilkubeconfig.GenerateSecret(cluster, clusterKubeconfig)
		if err := cl.Create(context.Background(), secret); err != nil {
			return fileList, err
		}
	}

	// Wait for the load balancer to be ready by checking the control plane endpoint
	// on the cluster object.
	var cluster *clusterv1.Cluster
	{
		if err := wait.ExponentialBackoff(wait.Backoff{
			Duration: time.Second * 10,
			Factor:   float64(1.5),
			Steps:    32,
		}, func() (bool, error) {
			c := &clusterv1.Cluster{}
			if err := cl.Get(context.Background(), client.ObjectKey{
				Name:      clusterID.InfraID,
				Namespace: capiutils.Namespace,
			}, c); err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}
				return false, err
			}
			cluster = c
			return cluster.Status.InfrastructureReady, nil
		}); err != nil {
			return fileList, err
		}
		if cluster == nil {
			return fileList, fmt.Errorf("error occurred during load balancer ready check")
		}
		if cluster.Spec.ControlPlaneEndpoint.Host == "" {
			return fileList, fmt.Errorf("control plane endpoint is not set")
		}
	}

	infraReadyInput := InfraReadyInput{
		Client:        cl,
		InstallConfig: installConfig,
	}

	if p, ok := i.capiProvider.(InfraReadyProvider); ok {
		if err := p.InfraReady(infraReadyInput); err != nil {
			return fileList, fmt.Errorf("failed provisioning resources after control plane available: %w", err)
		}
	}

	// For each manifest we created, retrieve it and store it in the asset.
	for _, m := range manifests {
		key := client.ObjectKey{
			Name:      m.GetName(),
			Namespace: m.GetNamespace(),
		}
		if err := cl.Get(context.Background(), key, m); err != nil {
			return fileList, fmt.Errorf("failed to get manifest: %w", err)
		}

		gvk, err := cl.GroupVersionKindFor(m)
		if err != nil {
			return fileList, fmt.Errorf("failed to get GVK for manifest: %w", err)
		}
		fileName := fmt.Sprintf("%s-%s-%s.yaml", gvk.Kind, m.GetNamespace(), m.GetName())
		objData, err := yaml.Marshal(m)
		if err != nil {
			return fileList, fmt.Errorf("failed to create infrastructure manifest %s from InstallConfig: %w", fileName, err)
		}
		fileList = append(fileList, &asset.File{
			Filename: fileName,
			Data:     objData,
		})
	}

	logrus.Infof("Cluster API resources have been created. Waiting for cluster to become ready...")
	return fileList, nil
}

// DestroyBootstrap destroys the temporary bootstrap resources.
func (i InfraProvider) DestroyBootstrap(dir string) error {
	metadata, err := metadata.Load(dir)
	if err != nil {
		return err
	}

	// TODO(padillon): start system if not running
	if sys := clusterapi.System(); sys.State() == clusterapi.SystemStateRunning {
		if err := sys.Client().Delete(context.TODO(), &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      capiutils.GenerateBoostrapMachineName(metadata.InfraID),
				Namespace: capiutils.Namespace,
			},
		}); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("failed to delete bootstrap machine: %w", err)
		}
	}
	return nil
}

// ExtractHostAddresses extracts the IPs of the bootstrap and control plane machines.
func (i InfraProvider) ExtractHostAddresses(dir string, config *types.InstallConfig, ha *infrastructure.HostAddresses) error {
	return nil
}

// DefaultCAPIProvider provides a base implementation of default behaviors which should be embedded
// by platform-specific provider implementations.
type DefaultCAPIProvider struct{}

// PreProvision is the default CAPI provider's implementation of the PreProvision interface. It does nothing.
func (d DefaultCAPIProvider) PreProvision(in PreProvisionInput) error {
	logrus.Debugf("Default PreProvision: doing nothing")
	return nil
}

// Ignition provides the default CAPI provider implementation for generating ignition secrets.
// It places the bootstrap and control plane ignition in secrets.
func DefaultIgnition(in IgnitionInput) ([]client.Object, error) {
	logrus.Debugf("Using default ignition secret generation")
	ignSecrets := []client.Object{}
	masterIgn := string(in.MasterIgnData)
	bootstrapIgn, err := injectInstallInfo(in.BootstrapIgnData)
	if err != nil {
		return nil, fmt.Errorf("unable to inject installation info: %w", err)
	}
	ignSecrets = append(ignSecrets,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-%s", in.InfraID, "master"),
				Namespace: capiutils.Namespace,
				Labels: map[string]string{
					"cluster.x-k8s.io/cluster-name": in.InfraID,
				},
			},
			Data: map[string][]byte{
				"format": []byte("ignition"),
				"value":  []byte(masterIgn),
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-%s", in.InfraID, "bootstrap"),
				Namespace: capiutils.Namespace,
				Labels: map[string]string{
					"cluster.x-k8s.io/cluster-name": in.InfraID,
				},
			},
			Data: map[string][]byte{
				"format": []byte("ignition"),
				"value":  []byte(bootstrapIgn),
			},
		},
	)
	return ignSecrets, nil
}

// InfraReady provides the default CAPI provider's implementation of the InfraReady
// function. It does nothing.
func (d DefaultCAPIProvider) InfraReady(in InfraReadyInput) error {
	logrus.Debugf("Default InfraReady, doing nothing")
	return nil
}
