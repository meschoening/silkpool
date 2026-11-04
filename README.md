# Silkpool

Silkpool puts spare computers on duty as self-hosted GitHub Actions runners, and takes them off duty without interrupting builds.

- **Installing it:** the [operator docs](docs/README.md) say how to install a Hub and enroll nodes from this repository's [releases](https://github.com/meschoening/silkpool/releases).
- **The source:** this repository holds Silkpool's source as each release was built from it, with one commit per release. It's developed and tested elsewhere, so it carries no tests.
- **How a release is built:** [the release workflow](.github/workflows/release.yml) builds each release from the source at its tag: the node's and the Hub's `.deb` and `.rpm` packages for amd64 and arm64, and `install.sh`, with `SHA256SUMS` beside them. To build them yourself, with any Go from 1.21 on, which fetches the toolchain `go.mod` names:

  ```sh
  go run ./packaging/build -version 1.2.3 -out dist
  ```

Silkpool is released under the [MIT License](LICENSE).
