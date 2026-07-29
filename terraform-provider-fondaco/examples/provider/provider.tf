terraform {
  required_providers {
    registry = {
      source = "registry.local/fondaco-dev/fondaco"
    }
  }
}

# The endpoint and the credential come from the environment
# (FONDACO_ENDPOINT, FONDACO_TOKEN) so a token never has to be written into
# a .tf file. The identity must match one of the site's `admins` patterns.
provider "registry" {}
