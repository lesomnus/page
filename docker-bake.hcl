# Match the image build contract used by lesomnus/cr. CI supplies TAG,
# BUILD_HASH and BUILD_ID; APP_VERSION is derived unless a release sets it.
variable "TAG" {
  default = "local"
}

variable "REPO" {
  default = "ghcr.io/lesomnus/page"
}

variable "BUILD_HASH" {
  default = "0000000000000000000000000000000000000000"
}

variable "BUILD_TIMESTAMP" {
  default = "${timestamp()}"
}

variable "BUILD_DATE" {
  default = "${formatdate("YYMMDD", BUILD_TIMESTAMP)}"
}

variable "BUILD_ID" {
  default = "r0"
}

variable "APP_VERSION" {
  default = "${BUILD_DATE}-${BUILD_ID}"
}

variable "PLATFORMS" {
  default = ["linux/amd64", "linux/arm64"]
}

# TAG follows the current branch/release, BUILD_ID identifies the CI run,
# BUILD_DATE follows the day, and BUILD_DATE-BUILD_ID identifies this build.
function "tags" {
  params = [name]
  result = [
    "${name}:${TAG}",
    "${name}:${BUILD_ID}",
    "${name}:${BUILD_DATE}",
    "${name}:${BUILD_DATE}-${BUILD_ID}",
  ]
}

target "app" {
  context    = "."
  dockerfile = "Dockerfile"
  target     = "app"
  platforms  = PLATFORMS
  tags       = tags(REPO)

  args = {
    APP_VERSION = APP_VERSION
  }

  labels = {
    "org.opencontainers.image.title"       = "page"
    "org.opencontainers.image.description" = "Static hosting with source-owned deployment and preview retention"
    "org.opencontainers.image.source"      = "https://github.com/lesomnus/page"
    "org.opencontainers.image.url"         = "https://github.com/lesomnus/page"
    "org.opencontainers.image.revision"    = BUILD_HASH
    "org.opencontainers.image.version"     = APP_VERSION
    "org.opencontainers.image.created"     = BUILD_TIMESTAMP
  }
}

group "default" {
  targets = ["app"]
}
