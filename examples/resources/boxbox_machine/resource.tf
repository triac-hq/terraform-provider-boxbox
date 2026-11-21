resource "boxbox_machine" "web" {
  name   = "web1"
  image  = "ubuntu-24.04"
  memory = "4GiB"
  disk   = "40GiB"
  cpu = {
    min = 1
    max = 4
  }
  ssh_keys = ["ssh-ed25519 AAAA... you@laptop"]
}
