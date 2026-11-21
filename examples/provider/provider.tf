# Credentials come from the environment:
#   BOXBOX_ENDPOINT              https://cluster.example.com
#   BOXBOX_API_TOKEN             <tenant>@pve!<tokenid>=<secret>   (PVE API token)
#   BOXBOX_ACCESS_CLIENT_ID      <id>.access                       (Cloudflare Access service token)
#   BOXBOX_ACCESS_CLIENT_SECRET  <secret>
provider "boxbox" {}
