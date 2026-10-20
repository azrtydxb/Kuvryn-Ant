# Getting help

Kuvryn Ant is a community-supported open-source project. There is no commercial
support offering.

- **Documentation:** <https://azrtydxb.github.io/Kuvryn-Ant/> covers installation,
  every package format, identity, replication and operations. Start with the
  [FAQ](https://azrtydxb.github.io/Kuvryn-Ant/about/faq).
- **Questions and ideas:** open a
  [GitHub Discussion](https://github.com/azrtydxb/Kuvryn-Ant/discussions) when
  discussions are enabled, otherwise an issue labelled `question`.
- **Bugs:** open an issue with the
  [bug report template](https://github.com/azrtydxb/Kuvryn-Ant/issues/new/choose).
  Attach the output of `ant diagnostics -o diagnostics.json` after checking it for
  anything you do not want to share.
- **New formats or clients:** use the format-support request template.
- **Security vulnerabilities:** never in public; follow [SECURITY.md](SECURITY.md).

When asking for help, include the Kuvryn Ant version or commit, how it is deployed
(binary, Compose, Helm), the storage backend, the package client and its version,
and the exact command and response (status code and the `X-Kuvryn-Cache` header if
present). Replace real hostnames and tokens with placeholders.
