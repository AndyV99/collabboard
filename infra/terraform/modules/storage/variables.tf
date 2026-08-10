variable "name_prefix" {
  description = "Prefix for every resource name, e.g. `collabboard-staging`."
  type        = string
}

variable "bucket_suffix" {
  description = "Appended to the bucket name to make it globally unique. The AWS account ID, supplied by the caller."
  type        = string
}

variable "kms_key_arn" {
  description = "CMK encrypting objects at rest."
  type        = string
}

variable "noncurrent_version_expiration_days" {
  description = "How long a superseded attachment version is retained before deletion."
  type        = number
  default     = 30
}

variable "force_destroy" {
  description = "Allow `terraform destroy` to delete a bucket that still has objects in it. True only where the contents are disposable."
  type        = bool
  default     = false
}
