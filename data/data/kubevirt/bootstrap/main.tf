provider "kubernetes" {
}

provider "kubevirt" {
}

data "ignition_file" "hostname" {
  mode = "420"
  path = "/etc/hostname"

  content {
    content = <<EOF
${var.cluster_id}-bootstrap
EOF
  }
}

data "ignition_config" "bootstrap_ignition_config" {

  merge {
    source = "data:text/plain;charset=utf-8;base64,${base64encode(var.ignition_bootstrap)}"
  }

  files = [
    element(data.ignition_file.hostname.*.rendered, 0)
  ]
}

resource "kubernetes_secret" "bootstrap_ignition" {
  metadata {
    name = "${var.cluster_id}-bootstrap-ignition"
    namespace = var.kubevirt_namespace
    labels = var.kubevirt_labels
  }
  data = {
    "userdata" = element(
      data.ignition_config.bootstrap_ignition_config.*.rendered,
      0,
    )
  }
}

resource "kubevirt_virtual_machine" "bootstrap_vm" {

  metadata {
    name = "${var.cluster_id}-bootstrap"
    namespace = var.kubevirt_namespace
    labels = var.kubevirt_labels
  }
  spec {
    run_strategy = "Always"
    data_volume_templates {
      metadata {
        name = "${var.cluster_id}-bootstrap-bootvolume"
        namespace = var.kubevirt_namespace
      }
      spec {
        source {
          pvc {
            name = var.kubevirt_source_pvc_name
            namespace = var.kubevirt_namespace
          }
        }
        pvc {
          access_modes = [var.kubevirt_pv_access_mode]
          resources {
            requests = {
              storage = var.storage
            }
          }
          storage_class_name = var.kubevirt_storage_class
        }
      }
    }
    template {
      metadata {
        labels = {
          "kubevirt.io/vm" = "${var.cluster_id}-bootstrap"
        }
      }
      spec {
        volume {
          name = "datavolumedisk1"
          volume_source {
            data_volume {
              name = "${var.cluster_id}-bootstrap-bootvolume"
            }
          }
        }
        volume {
          name = "cloudinitdisk"
          volume_source {
            cloud_init_config_drive {
              user_data_secret_ref {
                name = kubernetes_secret.bootstrap_ignition.metadata[0].name
              }
            }
          }
        }
        domain {
          resources {
            requests = {
              memory = var.memory
              cpu = var.cpu
            }
          }
          devices {
            disk {
              name = "datavolumedisk1"
              disk_device {
                disk {
                  bus = "virtio"
                }
              }
            }
            disk {
              name = "cloudinitdisk"
              disk_device {
                disk {
                  bus = "virtio"
                }
              }
            }
            interface {
              name = "main"
              interface_binding_method = var.kubevirt_interface_binding_method
            }
          }
        }
        network {
          name = "main"
          network_source {
            multus {
              network_name = var.kubevirt_network_name
            }
          }
        }
      }
    }
  }
}
