# ---------------------------------------------------------------------------
# IAM: one IRSA role per NEXUS service account, plus a reusable push policy
# for whatever identity CI runs as.
#
# IRSA (IAM Roles for Service Accounts) exchanges the pod's projected service
# account token for AWS credentials through the cluster's OIDC provider. The
# alternative - node instance profiles - grants every pod on a node the union
# of every permission any pod on it needs, which is not a boundary at all.
# ---------------------------------------------------------------------------

locals {
  irsa_enabled = var.eks_oidc_provider_arn != null

  irsa_roles = local.irsa_enabled ? toset(var.irsa_service_accounts) : toset([])
}

data "aws_iam_policy_document" "irsa_assume_role" {
  for_each = local.irsa_roles

  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [var.eks_oidc_provider_arn]
    }

    # Both conditions matter. Without the :sub check any service account in the
    # cluster could assume the role; without the :aud check a token minted for
    # a different audience would be accepted.
    condition {
      test     = "StringEquals"
      variable = "${var.eks_oidc_provider_url}:sub"
      values   = ["system:serviceaccount:${var.kubernetes_namespace}:${each.value}"]
    }

    condition {
      test     = "StringEquals"
      variable = "${var.eks_oidc_provider_url}:aud"
      values   = ["sts.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "irsa" {
  for_each = local.irsa_roles

  name        = "${local.identifier}-${each.value}"
  description = "IRSA role for the ${each.value} service account in the ${var.kubernetes_namespace} namespace"

  assume_role_policy = data.aws_iam_policy_document.irsa_assume_role[each.value].json

  # One hour. Long enough that credential refresh is not a hot path, short
  # enough that a leaked token has a bounded life.
  max_session_duration = 3600

  tags = merge(local.common_tags, {
    "Name"           = "${local.identifier}-${each.value}"
    "ServiceAccount" = each.value
  })

  lifecycle {
    # eks_oidc_provider_url cannot be checked in a variable validation block
    # against another variable, so the pairing is enforced here instead. A role
    # created with a null URL would have a trust policy matching
    # ":sub", which no token can ever satisfy - it would fail silently at
    # runtime rather than loudly at plan time.
    precondition {
      condition     = var.eks_oidc_provider_url != null
      error_message = "eks_oidc_provider_url must be set whenever eks_oidc_provider_arn is set; the trust policy conditions are built from the issuer URL."
    }
  }
}

# ---------------------------------------------------------------------------
# Reading the database credential
# ---------------------------------------------------------------------------

data "aws_iam_policy_document" "read_database_secret" {
  count = local.irsa_enabled && var.store_password_in_secrets_manager ? 1 : 0

  statement {
    effect = "Allow"
    actions = [
      "secretsmanager:GetSecretValue",
      "secretsmanager:DescribeSecret",
    ]
    # Scoped to this one secret. A wildcard here would let any NEXUS pod read
    # every secret in the account.
    resources = [aws_secretsmanager_secret.database[0].arn]
  }

  # Decrypting the secret needs the key too, when it is a customer-managed one.
  dynamic "statement" {
    for_each = var.kms_key_arn != null ? [var.kms_key_arn] : []

    content {
      effect    = "Allow"
      actions   = ["kms:Decrypt"]
      resources = [statement.value]
    }
  }
}

resource "aws_iam_policy" "read_database_secret" {
  count = local.irsa_enabled && var.store_password_in_secrets_manager ? 1 : 0

  name        = "${local.identifier}-read-database-secret"
  description = "Read the NEXUS PostgreSQL credential from Secrets Manager"
  policy      = data.aws_iam_policy_document.read_database_secret[0].json

  tags = local.common_tags
}

resource "aws_iam_role_policy_attachment" "read_database_secret" {
  for_each = var.store_password_in_secrets_manager ? local.irsa_roles : toset([])

  role       = aws_iam_role.irsa[each.value].name
  policy_arn = aws_iam_policy.read_database_secret[0].arn
}

# ---------------------------------------------------------------------------
# Pushing images
# ---------------------------------------------------------------------------

data "aws_iam_policy_document" "ecr_push" {
  # GetAuthorizationToken has no resource-level permissions; it is account-wide
  # by design and only returns a token, which is then scoped by the statement
  # below.
  statement {
    effect    = "Allow"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }

  statement {
    effect = "Allow"
    actions = [
      "ecr:BatchCheckLayerAvailability",
      "ecr:CompleteLayerUpload",
      "ecr:InitiateLayerUpload",
      "ecr:PutImage",
      "ecr:UploadLayerPart",
      "ecr:BatchGetImage",
      "ecr:GetDownloadUrlForLayer",
      "ecr:DescribeImages",
      "ecr:DescribeRepositories",
    ]
    resources = [for r in aws_ecr_repository.this : r.arn]
  }
}

# Deliberately a standalone policy with no role attached. Who pushes images -
# a GitHub Actions OIDC role, a CodeBuild project, a human - is a decision for
# whoever owns CI, and this module has no business creating an identity on
# their behalf. Attach it with aws_iam_role_policy_attachment where that
# identity is defined.
resource "aws_iam_policy" "ecr_push" {
  name        = "${local.identifier}-ecr-push"
  description = "Push and pull NEXUS images in the ${local.identifier} ECR repositories"
  policy      = data.aws_iam_policy_document.ecr_push.json

  tags = local.common_tags
}
