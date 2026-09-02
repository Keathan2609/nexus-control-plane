# ---------------------------------------------------------------------------
# REMOTE STATE - COMMENTED OUT ON PURPOSE.
#
# This block is inert so that `tofu init -backend=false` and `tofu validate`
# work on any machine, including one with no AWS account. Uncommenting it is a
# deliberate step taken once the state bucket exists.
#
# Why remote state matters here and not in environments/local: this root module
# manages a database that holds the system of record. Local state means the
# only record of what exists lives on one laptop, two people applying at once
# corrupt each other's work silently, and a lost laptop means adopting every
# resource by hand.
#
# ---------------------------------------------------------------------------
# ENABLING IT
#
# 1. Create the bucket and the lock table ONCE, out of band. They cannot be
#    managed by the configuration whose state they hold - that is a bootstrap
#    cycle. Do it with the CLI or a separate tiny root module:
#
#      aws s3api create-bucket \
#        --bucket nexus-tfstate-<account-id> \
#        --region eu-west-1 \
#        --create-bucket-configuration LocationConstraint=eu-west-1
#
#      aws s3api put-bucket-versioning \
#        --bucket nexus-tfstate-<account-id> \
#        --versioning-configuration Status=Enabled
#
#      aws s3api put-bucket-encryption \
#        --bucket nexus-tfstate-<account-id> \
#        --server-side-encryption-configuration \
#        '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"aws:kms"}}]}'
#
#      aws s3api put-public-access-block \
#        --bucket nexus-tfstate-<account-id> \
#        --public-access-block-configuration \
#        BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
#
#      aws dynamodb create-table \
#        --table-name nexus-tfstate-lock \
#        --attribute-definitions AttributeName=LockID,AttributeType=S \
#        --key-schema AttributeName=LockID,KeyType=HASH \
#        --billing-mode PAY_PER_REQUEST \
#        --region eu-west-1
#
#    Versioning is not optional. It is the only way back from a bad state
#    write, and state files are exactly the kind of thing you want an undo for.
#
# 2. Uncomment the block below and set bucket, region and key.
#
# 3. Run `tofu init -migrate-state` once. It copies the existing local state
#    into S3 and asks for confirmation first.
#
# 4. Delete the now-stale local terraform.tfstate. It contains credentials and
#    is no longer authoritative.
#
# ---------------------------------------------------------------------------
# NOTES
#
# - `dynamodb_table` is the widely supported locking mechanism and the reason
#   the table above exists. S3 also supports native conditional-write locking
#   via `use_lockfile = true`, which removes the DynamoDB table entirely; if
#   your OpenTofu and AWS provider versions support it, prefer that and drop
#   the table. Do not run both halves of that migration in one change.
# - `key` includes the environment. One bucket per account with one key per
#   environment keeps prod and staging state separate while remaining a single
#   thing to secure.
# - Cost: a few cents a month. State files are kilobytes and locks are a
#   handful of writes per apply.
# ---------------------------------------------------------------------------

# terraform {
#   backend "s3" {
#     bucket = "nexus-tfstate-<account-id>"
#     key    = "nexus/production/terraform.tfstate"
#     region = "eu-west-1"
#
#     encrypt = true
#
#     # Object-level encryption with a customer-managed key, on top of the
#     # bucket default. Optional, but state is the highest-value object in the
#     # account and deserves its own key policy.
#     # kms_key_id = "arn:aws:kms:eu-west-1:<account-id>:key/<key-id>"
#
#     # State locking. Without it, two concurrent applies race and the loser's
#     # changes vanish from state while still existing in AWS.
#     dynamodb_table = "nexus-tfstate-lock"
#
#     # Newer alternative to the DynamoDB table above - S3 conditional writes.
#     # Enable one or the other, not both at once.
#     # use_lockfile = true
#   }
# }
