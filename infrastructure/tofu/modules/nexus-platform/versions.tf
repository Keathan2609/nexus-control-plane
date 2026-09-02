terraform {
  required_version = ">= 1.6.0"

  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.31"
    }
    # Pinned to 2.x on purpose. The 3.x provider reworked the release schema
    # (notably how `set` and the kubernetes connection block are expressed), so
    # a floating major here would break every caller on an unrelated `init`.
    # Moving to 3.x is a deliberate, tested change, not a version-range side
    # effect.
    helm = {
      source  = "hashicorp/helm"
      version = "~> 2.15"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}
