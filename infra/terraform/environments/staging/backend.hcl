# Account-specific half of the S3 backend configuration, supplied at init:
#
#   terraform init -backend-config=backend.hcl
#
# Both values are printed ready to paste by the bootstrap stack:
#
#   cd ../../bootstrap && terraform output -raw backend_config
#
# Neither is a secret -- a bucket name and a key ARN grant nothing on their own.
# `kms_key_id` must be the key ARN, not the `alias/...` name: the backend
# rejects an alias, and the state bucket's policy compares the encryption header
# against this exact ARN, so an alias here would 403 every state write.
#
# Replace both placeholders. `init` fails loudly against a bucket that does not
# exist, which is the intended behaviour if this was never filled in.

bucket     = "collabboard-tfstate-REPLACE_WITH_AWS_ACCOUNT_ID"
kms_key_id = "arn:aws:kms:us-east-1:REPLACE_WITH_AWS_ACCOUNT_ID:key/REPLACE_WITH_STATE_KEY_UUID"
