# terraform-provider-boxbox

A Terraform / OpenTofu provider for the tenant API of
[boxbox3](https://github.com/triac-hq), a tool that runs a Proxmox VE cluster for
several tenants. A tenant creates, changes and destroys its own virtual machines within the
quota the cluster's owner gave it.

The provider talks only to boxbox3's HTTP API (`/boxbox/api/v1/`). Quota, capacity, node
placement and permissions are decided by the cluster, not by the provider.

```hcl
terraform {
  required_providers {
    boxbox = {
      source = "triac-hq/boxbox"
    }
  }
}

provider "boxbox" {}

resource "boxbox_machine" "web" {
  name     = "web1"
  image    = "ubuntu-24.04"
  memory   = "4GiB"
  disk     = "40GiB"
  cpu      = { min = 1, max = 4 }
  ssh_keys = ["ssh-ed25519 AAAA... you@laptop"]
}
```

## Credentials

The cluster's owner gives you two credentials; both are read from the environment and never
stored in the state.

| Variable | What |
|---|---|
| `BOXBOX_ENDPOINT` | the cluster's address, e.g. `https://cluster.example.com` |
| `BOXBOX_API_TOKEN` | your PVE API token, `<tenant>@pve!<tokenid>=<secret>` |
| `BOXBOX_ACCESS_CLIENT_ID`, `BOXBOX_ACCESS_CLIENT_SECRET` | the Cloudflare Access service token that lets automation past the edge |

## Behaviour worth knowing

- Creating a machine waits until its first boot is done (typically 2-10 minutes).
- A change of `cpu.max` or `memory` on a running machine takes effect when the machine is next
  stopped and started; the provider warns, and the cluster never restarts a machine by itself.
- A disk never shrinks. Changing `name` or `image` replaces the machine.
- `tailscale_auth_key` and `password` are write-only: they are not kept in the plan or state.

Documentation of every resource and data source: [`docs/`](docs/).

## Development

```sh
go test ./...        # acceptance tests run the real tofu/terraform against an in-process fake API
```

License: MPL-2.0.
