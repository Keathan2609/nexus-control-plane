terraform {
  required_version = ">= 1.6.0"

  required_providers {
    # Pinned to the 5.x major. AWS provider majors routinely change resource
    # defaults and occasionally require state migrations; that is a scheduled
    # piece of work, not something to discover during an unrelated apply.
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.60"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}
