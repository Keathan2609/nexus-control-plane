# ---------------------------------------------------------------------------
# One ECR repository per NEXUS image.
#
# Separate repositories rather than one repository with tag prefixes: lifecycle
# policies, scan findings and repository policies are all per-repository, so a
# shared repository would force the frontend and the control plane to have the
# same retention and the same readers.
# ---------------------------------------------------------------------------

resource "aws_ecr_repository" "this" {
  for_each = toset(var.ecr_repositories)

  name                 = "${var.name_prefix}/${each.value}"
  image_tag_mutability = var.ecr_image_tag_mutability
  force_delete         = var.ecr_force_delete

  image_scanning_configuration {
    scan_on_push = var.ecr_scan_on_push
  }

  encryption_configuration {
    # KMS when a key is supplied, otherwise AES256. AES256 is free and still
    # encrypts at rest; a customer-managed key buys auditable key access and
    # the ability to revoke it, at roughly a dollar a month per key plus
    # request charges.
    encryption_type = var.kms_key_arn != null ? "KMS" : "AES256"
    kms_key         = var.kms_key_arn
  }

  tags = merge(local.common_tags, {
    "Name"      = "${var.name_prefix}/${each.value}"
    "Component" = each.value
  })
}

# Untagged images are build layers nobody can deploy; they are pure cost after
# a day. Tagged images are kept by count so a rollback target always exists.
resource "aws_ecr_lifecycle_policy" "this" {
  for_each = aws_ecr_repository.this

  repository = each.value.name

  policy = jsonencode({
    rules = [
      {
        rulePriority = 1
        description  = "Expire untagged images after 1 day"
        selection = {
          tagStatus   = "untagged"
          countType   = "sinceImagePushed"
          countUnit   = "days"
          countNumber = 1
        }
        action = {
          type = "expire"
        }
      },
      {
        rulePriority = 2
        description  = "Keep the most recent ${var.ecr_keep_last_images} images"
        selection = {
          tagStatus   = "any"
          countType   = "imageCountMoreThan"
          countNumber = var.ecr_keep_last_images
        }
        action = {
          type = "expire"
        }
      },
    ]
  })
}
