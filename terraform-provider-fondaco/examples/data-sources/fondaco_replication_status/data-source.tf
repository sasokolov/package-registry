data "registry_replication_status" "here" {}

# The durability watermark is the real RPO: everything at or below it
# survives losing this site.
output "durable_watermarks" {
  value = {
    for cursor in data.registry_replication_status.here.cursors :
    cursor.peer => cursor.durable_seq
  }
}
