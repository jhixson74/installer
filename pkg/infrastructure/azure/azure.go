package azure

import (
	"github.com/openshift/installer/pkg/infrastructure/clusterapi"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Provider struct{}

func (p Provider) Ignition(in clusterapi.IgnitionInput) ([]client.Object, error) {
	return clusterapi.DefaultIgnition(in)
}
