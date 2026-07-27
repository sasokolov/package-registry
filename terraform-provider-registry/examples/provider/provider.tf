terraform {
  required_providers {
    registry = {
      source = "registry.local/sasokolov/registry"
    }
  }
}

# The endpoint and the credential come from the environment
# (REGISTRY_ENDPOINT, REGISTRY_TOKEN) so a token never has to be written into
# a .tf file. The identity must match one of the site's `admins` patterns.
provider "registry" {}
