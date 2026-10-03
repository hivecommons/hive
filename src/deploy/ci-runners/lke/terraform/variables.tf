variable "cluster_label" {
  type    = string
  default = "hive-ci"
}

variable "region" {
  type        = string
  default     = "us-ord" # Chicago — Dedicated CPU plans without availability limits
}

variable "k8s_version" {
  type    = string
  default = "1.36"
}

variable "system_pool_type" {
  type        = string
  default     = "g8-dedicated-8-4" # G8 Dedicated 8x4: 4 vCPU / 8 GB / 82 GB
  description = "Plan for the ARC controller/listener/registry-cache pool."
}

variable "system_pool_count" {
  type    = number
  default = 3 # LKE recommends 3 for HA of the system pool
}

variable "runner_pool_type" {
  type        = string
  default     = "g8-dedicated-64-32" # G8 Dedicated 64x32: 32 vCPU / 64 GB / 655 GB NVMe
  description = "Plan for runner pods. ~5 runners per node at 6 CPU / 24 Gi worst case."
}

variable "runner_pool_count" {
  type    = number
  default = 4 # initial count; the autoscaler (3-12) takes over from here
}

variable "runner_pool_min" {
  type    = number
  default = 4 # matches the live pool floor; 3 was never applied (pool was hand-built with the autoscaler off)
}

variable "runner_pool_max" {
  type    = number
  default = 12 # 100 runners x 3.25 CPU requested / 32 vCPU ≈ 11 nodes
}

variable "control_plane_acl_cidrs" {
  type        = list(string)
  default     = []
  description = "IPv4 CIDRs allowed to reach the Kubernetes API. Empty disables the ACL."
}

variable "tags" {
  type    = list(string)
  default = ["hive", "ci"]
}
