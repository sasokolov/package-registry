# Block a version while an advisory is investigated. The bytes stay where
# they are; only access is removed, at this site and every site the block
# replicates to. Deleting this resource releases it.
resource "registry_quarantine" "cve_2026_0001" {
  feed       = "releases"
  coordinate = "maven:com.example:widget@1.4.2"
  detail     = "CVE-2026-0001, waiting on 1.4.3"
}
