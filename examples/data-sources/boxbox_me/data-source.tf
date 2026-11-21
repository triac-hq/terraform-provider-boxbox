data "boxbox_me" "me" {}

output "quota" {
  value = data.boxbox_me.me.quota
}
