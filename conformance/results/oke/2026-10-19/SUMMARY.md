# Conformance summary

Target: `oke` Ant: `git:06c3f70` Date: 2026-10-19

| Format      | Result | Pass | Fail | Documented limitation | Skip | Clients                                                                                                                                                                   |
| ----------- | ------ | ---- | ---- | --------------------- | ---- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| ansible     | FAIL   | 8    | 2    | 0                     | 1    | python `ansible-galaxy [core 2.21.5]`                                                                                                                                     |
| apk         | FAIL   | 8    | 1    | 0                     | 1    | alpine `apk-tools 3.0.8-r0, compiled for aarch64.`                                                                                                                        |
| apt         | FAIL   | 10   | 1    | 0                     | 0    | debian `apt 3.0.3 (arm64)`                                                                                                                                                |
| cargo       | PASS   | 11   | 0    | 0                     | 0    | cargo `cargo 1.99.1 (5f94df478 2026-08-27)`                                                                                                                               |
| cocoapods   | FAIL   | 10   | 1    | 0                     | 1    | ruby `1.17.0`                                                                                                                                                             |
| composer    | PASS   | 10   | 0    | 0                     | 0    | composer `Composer version 2.10.3 2026-08-27 13:34:23`                                                                                                                    |
| conan       | PASS   | 10   | 0    | 0                     | 0    | python `Conan version 2.33.0`                                                                                                                                             |
| conda       | PASS   | 11   | 0    | 0                     | 0    | conda `conda 26.7.2 / mamba 2.9.0`                                                                                                                                        |
| cran        | PASS   | 10   | 0    | 0                     | 0    | r `R version 4.6.1 (2026-06-24) -- "Happy Hop"`                                                                                                                           |
| docker      | PASS   | 14   | 0    | 0                     | 0    | crane `8a72a424fdecb4caa14f2d525e5d2503331442b5`                                                                                                                          |
| gitlfs      | PASS   | 8    | 0    | 0                     | 1    | alpine `git-lfs/3.7.1 (GitHub; linux arm64; go 1.26.9)`                                                                                                                   |
| go          | PASS   | 10   | 0    | 0                     | 0    | go `go version go1.27.2 linux/arm64`                                                                                                                                      |
| helm        | PASS   | 13   | 0    | 0                     | 0    | helm `v4.3.1+gc0d08d0`                                                                                                                                                    |
| huggingface | FAIL   | 8    | 1    | 0                     | 0    | python `huggingface_hub 2.2.0`                                                                                                                                            |
| maven       | PASS   | 10   | 0    | 1                     | 0    | maven `Apache Maven 3.10.0 (c43a36b8d67be7e0805a411bc0898af1a51f5472)`                                                                                                    |
| npm         | PASS   | 10   | 0    | 1                     | 0    | node `npm notice`                                                                                                                                                         |
| nuget       | FAIL   | 11   | 2    | 0                     | 0    | dotnet `10.0.402`                                                                                                                                                         |
| oras        | PASS   | 9    | 0    | 0                     | 0    | oras `Version:        1.3.4`                                                                                                                                              |
| p2          | PASS   | 7    | 0    | 0                     | 2    | curl `curl 8.11.1 (aarch64-unknown-linux-musl) libcurl/8.11.1 OpenSSL/3.3.2 zlib/1.3.1 brotli/1.1.0 zstd/1.5.6 libidn2/2.3.7 libpsl/0.21.5 libssh2/1.11.1 nghttp2/1.64.0` |
| pub         | PASS   | 11   | 0    | 0                     | 0    | dart `Dart SDK version: 3.13.5 (stable) (Tue Sep 29 01:00:52 2026 -0700) on "linux_arm64"`                                                                                |
| pypi        | FAIL   | 15   | 1    | 0                     | 0    | python `pip 26.2.1 / twine twine version 7.0.0 (readme-renderer: 46.0, requests: 2.34.2, requests- / Python 3.14.8`                                                       |
| raw         | PASS   | 12   | 0    | 0                     | 0    | curl `curl 8.11.1 (aarch64-unknown-linux-musl) libcurl/8.11.1 OpenSSL/3.3.2 zlib/1.3.1 brotli/1.1.0 zstd/1.5.6 libidn2/2.3.7 libpsl/0.21.5 libssh2/1.11.1 nghttp2/1.64.0` |
| rpm         | FAIL   | 8    | 1    | 0                     | 1    | fedora `dnf5 version 5.4.3.0`                                                                                                                                             |
| rubygems    | PASS   | 12   | 0    | 0                     | 0    | ruby `RubyGems 4.0.20 / ruby 4.0.7 / Bundler 4.0.20`                                                                                                                      |
| swift       | FAIL   | 7    | 2    | 0                     | 1    | swift `Swift version 6.4.1 (swift-6.4.1-RELEASE)`                                                                                                                         |
| terraform   | FAIL   | 8    | 3    | 0                     | 1    | terraform `Terraform v1.16.6`                                                                                                                                             |

### ansible

- **fail** `publish: ansible-galaxy collection build + publish 1.0.0 (native client)`: exit status 1: Created collection for kcdemo.collc9c149 at /work/dist/kcdemo-collc9c149-1.0.0.tar.gz [ERROR]: Error when publishing collection to cmd_arg (https://ant.azrty.com/feeds/cf-ansible-local-c9c149/ansible/) (HTTP Code: 400, Message: Bad Request Code: Unknown) Publishing collection artifact '/work/dist/kcdemo-collc9c149-1.0.0.tar.gz' to cmd_arg https://ant.azrty.com/feeds/cf-ansible-local
- **fail** `remote pull-through miss: ansible-galaxy install ansible.posix:1.5.4 from galaxy.ansible.com via feed`: exit status 1: [WARNING]: The specified collections path '/work/rinst' is not part of the configured Ansible collections paths '/work/home/.ansible/collections:/usr/share/ansible/collections'. The installed collection will not be picked up in an Ansible run, unless within a playbook-adjacent collections directory. [ERROR]: Failed to download collection tar from 'cmd_arg': HTTP Error 503: Service U

### apk

- **fail** `edge: a noarch package listed in the arch index is downloadable (apk requests it under the noarch path)`: exit status 1: Kuvryn Ant v3.24/main/aarch64 [https://service:*@ant.azrty.com/feeds/cf-apk-local-29ad02/apk/v3.24/main] OK: 18 distinct packages available (1/1) Installing kcdemo29ad02na (1.0-r0) ERROR: kcdemo29ad02na-1.0-r0: HTTP 404: Not Found 1 error; 8435 KiB in 16 packages

### apt

- **fail** `edge: package built with the dpkg-deb default compression (zstd on trixie) is accepted`: PUT /feeds/cf-apt-local-29ad02/apt/pool/main/k/kcdemo29ad02/kcdemo29ad02_1.0_all.deb -> 400, want [200 201 204]: {"error":{"code":"400","message":"Bad Request"}}

### cocoapods

- **fail** `remote pull-through miss: pod install AFNetworking 4.0.1 resolved through the feed (specs from cdn.cocoapods.org, source from github)`: exit status 1: tworking.podspec.json Response: 503 Service Unavailable CDN: antr URL couldn't be downloaded: https://service:***REDACTED***@ant.azrty.com/feeds/cf-cocoapods-remote-e725b4/cocoapods/Specs/a/7/5/AFNetworking/3.0.2/AFNetworking.podspec.json Response: 503 Service Unavailable CDN: antr URL couldn't be downloaded: https://service:***REDACTED***@ant.azrty.com/feeds/cf-cocoapods-remote-e

### huggingface

- **fail** `consume: hf download config.json and snapshot_download from the feed`: exit status 1: [90mHint: The `hf-cli` skill is not installed. Run `hf skills add -g` to teach your AI agents how to use the `hf` CLI.[0m [32m✓ Downloaded[0m path: /work/dl/config.json Traceback (most recent call last): File "<string>", line 1, in <module> from huggingface_hub import snapshot_download as s; p=s('kcdemo/model-0c44be', cache_dir='/work/hfsnap'); import os; print(sorted(

### maven

- **unsupported** `publish: mvn deploy:deploy-file (native multi-PUT)`: documented limitation (internal/maven README: native Maven deploy multi-PUT returns 405; release bundles via POST version/_bundle): exit status 1: prefixes-apache.snapshots.txt) [INFO] [INFO] ------------------< org.apache.maven:standalone-pom >------------------- [INFO] Building Maven Stub Project (No POM) 1 [INFO] --------------------------------[ pom ]--------------------------------- [INFO]

### npm

- **unsupported** `same-endpoint write then read: publish 0.1.1 with default (latest) tag`: documented limitation (internal/npmregistry README: initial publish tags immutable; a second version using latest conflicts): exit status 1: npm notice npm notice package: @kcdemo/pkg-bfd252@0.1.1 npm notice Tarball Contents npm notice 33B index.js npm notice 98B package.json npm notice Tarball Details npm notice name: @kcdemo/pkg-bfd252 npm notice version: 0.1.1 npm notice filename: kcdemo-pkg-bf

### nuget

- **fail** `publish: dotnet pack + dotnet nuget push 1.0.0 (unmodified pack output)`: exit status 1: Kc.Demo555c9a.1.0.0.nupkg Pushing Kc.Demo555c9a.1.0.0.nupkg to 'https://ant.azrty.com/feeds/cf-nuget-local-555c9a/nuget/v2/package'... PUT https://ant.azrty.com/feeds/cf-nuget-local-555c9a/nuget/v2/package/ BadRequest https://ant.azrty.com/feeds/cf-nuget-local-555c9a/nuget/v2/package/ 115ms error: Response status code does not indicate success: 400 (Bad Request).
- **fail** `publish: repack 1.0.0 without the nuspec BOM, then dotnet nuget push`: exit status 1: Pushing Kc.Demo555c9a.1.0.0.nupkg to 'https://ant.azrty.com/feeds/cf-nuget-local-555c9a/nuget/v2/package'... PUT https://ant.azrty.com/feeds/cf-nuget-local-555c9a/nuget/v2/package/ BadRequest https://ant.azrty.com/feeds/cf-nuget-local-555c9a/nuget/v2/package/ 247ms error: Response status code does not indicate success: 400 (Bad Request).

### pypi

- **fail** `edge: twine upload of a default setuptools wheel without long description`: exit status 1: * Getting build dependencies for sdist... * Building sdist... * Building wheel from sdist * Getting build dependencies for wheel... * Building wheel... Uploading distributions to https://ant.azrty.com/feeds/cf-pypi-local-f5a196/pypi/ Uploading kcdemo_nodesc_f5a196-0.1.0-py3-none-any.whl [?25l
  [2K[35m 0%[0m [90m━━━━━━━━━━━━━━━━━━━━━━

### rpm

- **fail** `remote pull-through miss: dnf download acl from the Rocky 9 BaseOS repo via feed`: exit status 1: a/8d3ab162394254bd62d6eb65ece6352448c0d1b7181abf932fb3bd514fa180bf-primary.xml.gz [HTTP/2 stream 1 was not closed cleanly: INTERNAL_ERROR (err 2)] - https://ant.azrty.com/feeds/cf-rpm-remote-29ad02/rpm/repodata/8d3ab162394254bd62d6eb65ece6352448c0d1b7181abf932fb3bd514fa180bf-primary.xml.gz >>> Curl error (92): Stream error in the HTTP/2 framing layer for https://ant.azrty.com/feeds/

### swift

- **fail** `publish: swift package-registry publish kcdemo.Lib29ad02 1.0.0 (native client)`: exit status 134: Object 0xffff7400f060 of class _MultiHandle deallocated with non-zero retain count 2. This object's deinit, or something called from it, may have created a strong reference to self which outlived deinit, resulting in a dangling reference. bash: line 1: 83 Aborted swift package-registry login https://ant.azrty.com/feeds/cf-swift-local-29ad02/swift --token "$TO
- **fail** `consume: swift package resolve with an id: dependency from the registry`: exit status 1: Object 0xffff9000cbe0 of class _MultiHandle deallocated with non-zero retain count 2. This object's deinit, or something called from it, may have created a strong reference to self which outlived deinit, resulting in a dangling reference. bash: line 1: 83 Aborted swift package-registry login https://ant.azrty.com/feeds/cf-swift-local-29ad02/swift --token "$TOKE

### terraform

- **fail** `consume: terraform init installs the provider (signature verified) from the feed`: exit status 1: Initializing the backend... Initializing provider plugins... - Finding ant.azrty.com/kcdemo/kc0c44be versions matching "1.0.0"... Error: Failed to install provider Error while installing ant.azrty.com/kcdemo/kc0c44be v1.0.0: could not query provider registry for ant.azrty.com/kcdemo/kc0c44be: failed to retrieve authentication checksums for provider: 401 Unauthorized returned from
- **fail** `same-endpoint write then read: publish provider 1.1.0 then terraform init -upgrade resolves it`: exit status 1: Initializing the backend... Initializing provider plugins... - Finding ant.azrty.com/kcdemo/kc0c44be versions matching ">= 1.0.0"... Error: Failed to install provider Error while installing ant.azrty.com/kcdemo/kc0c44be v1.1.0: could not query provider registry for ant.azrty.com/kcdemo/kc0c44be: failed to retrieve authentication checksums for provider: 401 Unauthorized returned f
- **fail** `remote pull-through miss: terraform init hashicorp/random 3.6.3 from registry.terraform.io via feed`: exit status 1: Initializing the backend... Initializing provider plugins... - Finding reg.example/hashicorp/random versions matching "3.6.3"... Error: Failed to install provider Error while installing reg.example/hashicorp/random v3.6.3: could not query provider registry for reg.example/hashicorp/random: the request failed after 2 attempts, please try again later: 503 Service Unavailable return
