# Rendered by control-install.sh (envsubst): ${PROM_PORT}.
# nomad-autoscaler agent config. Reads the scaling signal from Prometheus and
# resizes the worker MIG via the gce-mig target (ADC = the control VM's SA).
nomad {
  address = "http://127.0.0.1:4646"
}

apm "prometheus" {
  driver = "prometheus"
  config = {
    address = "http://127.0.0.1:${PROM_PORT}"
  }
}

target "gce-mig" {
  driver = "gce-mig"
}

strategy "pass-through" {
  driver = "pass-through"
}

policy {
  dir = "/etc/nomad-autoscaler/policies"
}
