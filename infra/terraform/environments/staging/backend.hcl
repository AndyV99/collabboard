# Account-specific half of the S3 backend configuration, supplied at init:
#
#   terraform init -backend-config=backend.hcl
#
# Both values are printed ready to paste by the bootstrap stack:
#
#   cd ../../bootstrap && terraform output -raw backend_config
#
# Neither is a secret -- a bucket name and a KMS alias grant nothing on their
# own. Replace the placeholder with the 12-digit AWS account ID; `init` fails
# loudly against a bucket that does not exist, which is the intended behaviour
# if this was never filled in.

bucket     = "collabboard-tfstate-REPLACE_WITH_AWS_ACCOUNT_ID"
kms_key_id = "alias/collabboard-tfstate"
