module "mymod" {
  source  = "registry.local/testns/mymod/generic"
  version = "2.0.0"
}

output "module_version" {
  value = module.mymod.version
}
