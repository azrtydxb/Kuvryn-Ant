# Conformance summary

Target: `kw` Ant: `git:06c3f70` Date: 2026-10-19

| Format      | Result | Pass | Fail | Documented limitation | Skip | Clients                                                                                                                                                                   |
| ----------- | ------ | ---- | ---- | --------------------- | ---- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| ansible     | FAIL   | 8    | 2    | 0                     | 1    | python `ansible-galaxy [core 2.21.5]`                                                                                                                                     |
| apk         | FAIL   | 8    | 1    | 0                     | 1    | alpine `apk-tools 3.0.8-r0, compiled for aarch64.`                                                                                                                        |
| apt         | FAIL   | 10   | 1    | 0                     | 0    | debian `apt 3.0.3 (arm64)`                                                                                                                                                |
| cargo       | PASS   | 11   | 0    | 0                     | 0    | cargo `cargo 1.99.1 (5f94df478 2026-08-27)`                                                                                                                               |
| cocoapods   | FAIL   | 10   | 1    | 0                     | 1    | ruby `1.17.0`                                                                                                                                                             |
| composer    | PASS   | 10   | 0    | 0                     | 0    | composer `PHP version 8.5.11 (/usr/local/bin/php)`                                                                                                                        |
| conan       | PASS   | 10   | 0    | 0                     | 0    | python `Conan version 2.33.0`                                                                                                                                             |
| conda       | PASS   | 11   | 0    | 0                     | 0    | conda `conda 26.7.2 / mamba 2.9.0`                                                                                                                                        |
| cran        | PASS   | 10   | 0    | 0                     | 0    | r `R version 4.6.1 (2026-06-24) -- "Happy Hop"`                                                                                                                           |
| docker      | FAIL   | 11   | 2    | 0                     | 1    | crane `8a72a424fdecb4caa14f2d525e5d2503331442b5`                                                                                                                          |
| gitlfs      | PASS   | 8    | 0    | 0                     | 1    | alpine `git-lfs/3.7.1 (GitHub; linux arm64; go 1.26.9)`                                                                                                                   |
| go          | PASS   | 10   | 0    | 0                     | 0    | go `go version go1.27.2 linux/arm64`                                                                                                                                      |
| helm        | PASS   | 13   | 0    | 0                     | 0    | helm `v4.3.1+gc0d08d0`                                                                                                                                                    |
| huggingface | FAIL   | 8    | 1    | 0                     | 0    | python `huggingface_hub 2.2.0`                                                                                                                                            |
| maven       | PASS   | 10   | 0    | 1                     | 0    | maven `Apache Maven 3.10.0 (c43a36b8d67be7e0805a411bc0898af1a51f5472)`                                                                                                    |
| npm         | PASS   | 10   | 0    | 1                     | 0    | node `npm notice`                                                                                                                                                         |
| nuget       | FAIL   | 12   | 1    | 0                     | 0    | dotnet `10.0.402`                                                                                                                                                         |
| oras        | FAIL   | 8    | 1    | 0                     | 0    | oras `Version:        1.3.4`                                                                                                                                              |
| p2          | PASS   | 7    | 0    | 0                     | 2    | curl `curl 8.11.1 (aarch64-unknown-linux-musl) libcurl/8.11.1 OpenSSL/3.3.2 zlib/1.3.1 brotli/1.1.0 zstd/1.5.6 libidn2/2.3.7 libpsl/0.21.5 libssh2/1.11.1 nghttp2/1.64.0` |
| pub         | PASS   | 11   | 0    | 0                     | 0    | dart `Dart SDK version: 3.13.5 (stable) (Tue Sep 29 01:00:52 2026 -0700) on "linux_arm64"`                                                                                |
| pypi        | FAIL   | 15   | 1    | 0                     | 0    | python `pip 26.2.1 / twine twine version 7.0.0 (readme-renderer: 46.0, requests: 2.34.2, requests- / Python 3.14.8`                                                       |
| raw         | PASS   | 12   | 0    | 0                     | 0    | curl `curl 8.11.1 (aarch64-unknown-linux-musl) libcurl/8.11.1 OpenSSL/3.3.2 zlib/1.3.1 brotli/1.1.0 zstd/1.5.6 libidn2/2.3.7 libpsl/0.21.5 libssh2/1.11.1 nghttp2/1.64.0` |
| rpm         | PASS   | 10   | 0    | 0                     | 0    | fedora `dnf5 version 5.4.3.0`                                                                                                                                             |
| rubygems    | PASS   | 12   | 0    | 0                     | 0    | ruby `RubyGems 4.0.20 / ruby 4.0.7 / Bundler 4.0.20`                                                                                                                      |
| swift       | FAIL   | 7    | 2    | 0                     | 1    | swift `Swift version 6.4.1 (swift-6.4.1-RELEASE)`                                                                                                                         |
| terraform   | FAIL   | 8    | 3    | 0                     | 1    | terraform `Terraform v1.16.6`                                                                                                                                             |

### ansible

- **fail** `publish: ansible-galaxy collection build + publish 1.0.0 (native client)`: exit status 1: Created collection for kcdemo.collf91492 at /work/dist/kcdemo-collf91492-1.0.0.tar.gz Publishing collection artifact '/work/dist/kcdemo-collf91492-1.0.0.tar.gz' to cmd_arg https://ant.kw.watteel.lab/feeds/cf-ansible-local-f91492/ansible/ [ERROR]: Error when publishing collection to cmd_arg (https://ant.kw.watteel.lab/feeds/cf-ansible-local-f91492/ansible/) (HTTP Code: 400, Message:
- **fail** `remote pull-through miss: ansible-galaxy install ansible.posix:1.5.4 from galaxy.ansible.com via feed`: exit status 1: [WARNING]: The specified collections path '/work/rinst' is not part of the configured Ansible collections paths '/work/home/.ansible/collections:/usr/share/ansible/collections'. The installed collection will not be picked up in an Ansible run, unless within a playbook-adjacent collections directory. [ERROR]: Failed to download collection tar from 'cmd_arg': HTTP Error 503: Service U

### apk

- **fail** `edge: a noarch package listed in the arch index is downloadable (apk requests it under the noarch path)`: exit status 1: Kuvryn Ant v3.24/main/aarch64 [https://service:*@ant.kw.watteel.lab/feeds/cf-apk-local-ca6517/apk/v3.24/main] OK: 18 distinct packages available (1/1) Installing kcdemoca6517na (1.0-r0) ERROR: kcdemoca6517na-1.0-r0: HTTP 404: Not Found 1 error; 8435 KiB in 16 packages

### apt

- **fail** `edge: package built with the dpkg-deb default compression (zstd on trixie) is accepted`: PUT /feeds/cf-apt-local-349c44/apt/pool/main/k/kcdemo349c44/kcdemo349c44_1.0_all.deb -> 400, want [200 201 204]: {"error":{"code":"400","message":"Bad Request"}}

### cocoapods

- **fail** `remote pull-through miss: pod install AFNetworking 4.0.1 resolved through the feed (specs from cdn.cocoapods.org, source from github)`: exit status 1: ice Unavailable CDN: antr URL couldn't be downloaded: https://service:***REDACTED***@ant.kw.watteel.lab/feeds/cf-cocoapods-remote-ccb4a4/cocoapods/Specs/a/7/5/AFNetworking/3.0.2/AFNetworking.podspec.json Response: 503 Service Unavailable CDN: antr URL couldn't be downloaded: https://service:***REDACTED***@ant.kw.watteel.lab/feeds/cf-cocoapods-remote-ccb4a4/cocoapods/Specs/a/7/5/AF

### docker

- **fail** `remote pull-through miss via registry-1.docker.io: crane digest+pull of library/hello-world`: exit status 1: 2026/10/18 22:13:49 logged in via /work/home/.docker/config.json 2026/10/18 22:13:49 retrying HEAD https://ant.kw.watteel.lab/v2/cf-docker-hub-91f7ec/library/hello-world/manifests/latest: unexpected status code 503 Service Unavailable (HEAD responses have no body, use GET for details) 2026/10/18 22:13:50 retrying HEAD https://ant.kw.watteel.lab/v2/cf-docker-hub-91f7ec/library/hello-
- **fail** `auth denial: /v2/ without or with bad token returns 401`: GET /v2/ -> 200, want [401]: {}

### huggingface

- **fail** `consume: hf download config.json and snapshot_download from the feed`: exit status 1: [90mHint: The `hf-cli` skill is not installed. Run `hf skills add -g` to teach your AI agents how to use the `hf` CLI.[0m [32m✓ Downloaded[0m path: /work/dl/config.json Traceback (most recent call last): File "<string>", line 1, in <module> from huggingface_hub import snapshot_download as s; p=s('kcdemo/model-349c44', cache_dir='/work/hfsnap'); import os; print(sorted(

### maven

- **unsupported** `publish: mvn deploy:deploy-file (native multi-PUT)`: documented limitation (internal/maven README: native Maven deploy multi-PUT returns 405; release bundles via POST version/_bundle): exit status 1: .snapshots.txt) [INFO] [INFO] ------------------< org.apache.maven:standalone-pom >------------------- [INFO] Building Maven Stub Project (No POM) 1 [INFO] --------------------------------[ pom ]--------------------------------- [INFO] [INFO] --- depl

### npm

- **unsupported** `same-endpoint write then read: publish 0.1.1 with default (latest) tag`: documented limitation (internal/npmregistry README: initial publish tags immutable; a second version using latest conflicts): exit status 1: npm notice npm notice package: @kcdemo/pkg-91f7ec@0.1.1 npm notice Tarball Contents npm notice 33B index.js npm notice 98B package.json npm notice Tarball Details npm notice name: @kcdemo/pkg-91f7ec npm notice version: 0.1.1 npm notice filename: kcdemo-pkg-91

### nuget

- **fail** `publish: dotnet pack + dotnet nuget push 1.0.0 (unmodified pack output)`: exit status 1: Kc.Demo8eedf4.1.0.0.nupkg Pushing Kc.Demo8eedf4.1.0.0.nupkg to 'https://ant.kw.watteel.lab/feeds/cf-nuget-local-8eedf4/nuget/v2/package'... PUT https://ant.kw.watteel.lab/feeds/cf-nuget-local-8eedf4/nuget/v2/package/ BadRequest https://ant.kw.watteel.lab/feeds/cf-nuget-local-8eedf4/nuget/v2/package/ 84ms error: Response status code does not indicate success: 400 (Bad Request).

### oras

- **fail** `auth denial: oras login with invalid credentials rejected`: expected failure but command succeeded: Login Succeeded

### pypi

- **fail** `edge: twine upload of a default setuptools wheel without long description`: exit status 1: * Getting build dependencies for sdist... * Building sdist... * Building wheel from sdist * Getting build dependencies for wheel... * Building wheel... Uploading distributions to https://ant.kw.watteel.lab/feeds/cf-pypi-local-91f7ec/pypi/ Uploading kcdemo_nodesc_91f7ec-0.1.0-py3-none-any.whl [?25l
  [2K[35m 0%[0m [90m━━━━━━━━━━━━━━━━━━━━�

### swift

- **fail** `publish: swift package-registry publish kcdemo.Libca6517 1.0.0 (native client)`: exit status 134: Object 0xffff9400bd80 of class _MultiHandle deallocated with non-zero retain count 2. This object's deinit, or something called from it, may have created a strong reference to self which outlived deinit, resulting in a dangling reference. bash: line 1: 86 Aborted swift package-registry login https://ant.kw.watteel.lab/feeds/cf-swift-local-ca6517/swift --token
- **fail** `consume: swift package resolve with an id: dependency from the registry`: exit status 1: Object 0xffff840327e0 of class _MultiHandle deallocated with non-zero retain count 2. This object's deinit, or something called from it, may have created a strong reference to self which outlived deinit, resulting in a dangling reference. bash: line 1: 85 Aborted swift package-registry login https://ant.kw.watteel.lab/feeds/cf-swift-local-ca6517/swift --token "

### terraform

- **fail** `consume: terraform init installs the provider (signature verified) from the feed`: exit status 1: Initializing the backend... Initializing provider plugins... - Finding ant.kw.watteel.lab/kcdemo/kc349c44 versions matching "1.0.0"... Error: Failed to install provider Error while installing ant.kw.watteel.lab/kcdemo/kc349c44 v1.0.0: could not query provider registry for ant.kw.watteel.lab/kcdemo/kc349c44: failed to retrieve authentication checksums for provider: 401 Unauthorize
- **fail** `same-endpoint write then read: publish provider 1.1.0 then terraform init -upgrade resolves it`: exit status 1: Initializing the backend... Initializing provider plugins... - Finding ant.kw.watteel.lab/kcdemo/kc349c44 versions matching ">= 1.0.0"... Error: Failed to install provider Error while installing ant.kw.watteel.lab/kcdemo/kc349c44 v1.1.0: could not query provider registry for ant.kw.watteel.lab/kcdemo/kc349c44: failed to retrieve authentication checksums for provider: 401 Unauthor
- **fail** `remote pull-through miss: terraform init hashicorp/random 3.6.3 from registry.terraform.io via feed`: exit status 1: Initializing the backend... Initializing provider plugins... - Finding reg.example/hashicorp/random versions matching "3.6.3"... Error: Failed to install provider Error while installing reg.example/hashicorp/random v3.6.3: could not query provider registry for reg.example/hashicorp/random: the request failed after 2 attempts, please try again later: 503 Service Unavailable return
