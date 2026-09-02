terraform {
  # 1.6 is the floor because everything here relies on it: the v1-suffixed
  # kubernetes resources, `moved` semantics we depend on for refactors, and
  # OpenTofu's own baseline. Nothing older is tested.
  required_version = ">= 1.6.0"

  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.31"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}
