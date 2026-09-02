# ---------------------------------------------------------------------------
# Providers for the cloud path.
#
# `tofu init -backend=false` and `tofu validate` work here without any
# credentials - provider configuration is not evaluated until plan. A plan or
# apply does need real AWS credentials and a reachable EKS cluster, which is
# the honest cost of managing real infrastructure and the reason this is not
# the default environment.
# ---------------------------------------------------------------------------

provider "aws" {
  region = var.aws_region

  # Credentials come from the ambient chain: SSO profile, environment
  # variables, or the CI role. Never from a variable, and never from a file in
  # this repository.

  default_tags {
    # Tags applied to every taggable resource in the account, so that anything
    # created here is attributable on a bill and findable in a sweep for
    # orphans.
    tags = {
      Application = "nexus"
      Environment = var.environment
      ManagedBy   = "opentofu"
      Repository  = "nexus"
    }
  }
}

# The cluster is an input, not something this root module creates. Cluster
# lifecycle (version upgrades, node group rotation) is a different cadence from
# application lifecycle, and coupling them means an application deploy can roll
# a control plane.
data "aws_eks_cluster" "this" {
  name = var.eks_cluster_name
}

provider "kubernetes" {
  host                   = data.aws_eks_cluster.this.endpoint
  cluster_ca_certificate = base64decode(data.aws_eks_cluster.this.certificate_authority[0].data)

  # exec, rather than the aws_eks_cluster_auth data source. That data source
  # resolves a bearer token at plan time, writes it into state, and hands you a
  # 15-minute expiry - so a long apply can fail halfway through with an
  # authentication error. The exec plugin fetches a fresh token when it is
  # actually needed and keeps it out of state entirely.
  exec {
    api_version = "client.authentication.k8s.io/v1beta1"
    command     = "aws"
    args = [
      "eks",
      "get-token",
      "--cluster-name", var.eks_cluster_name,
      "--region", var.aws_region,
    ]
  }
}

provider "helm" {
  kubernetes {
    host                   = data.aws_eks_cluster.this.endpoint
    cluster_ca_certificate = base64decode(data.aws_eks_cluster.this.certificate_authority[0].data)

    exec {
      api_version = "client.authentication.k8s.io/v1beta1"
      command     = "aws"
      args = [
        "eks",
        "get-token",
        "--cluster-name", var.eks_cluster_name,
        "--region", var.aws_region,
      ]
    }
  }
}
