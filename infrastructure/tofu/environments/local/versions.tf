terraform {
  required_version = ">= 1.6.0"

  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.31"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 2.15"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }

  # No backend block. State for the local environment stays in
  # terraform.tfstate next to this file, which is deliberate:
  #
  #   - It is the only configuration that works with no account anywhere.
  #   - The state describes a cluster that lives on one laptop. Sharing it
  #     would be actively misleading, because nobody else can reach the
  #     resources it describes.
  #
  # .gitignore keeps the state file out of git - it holds the generated
  # database password and JWT signing key in clear text.
  #
  # The remote backend example lives in environments/production/backend.tf.
}
