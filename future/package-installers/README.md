# Package installers — future work, NOT part of v0.1.0

**Nothing in this directory is published.** These are name-reservation drafts
kept out of the launch: publishing packages that merely point at the repo was
rejected during launch review. This directory is excluded from release
artifacts (goreleaser archives contain only the binary, LICENSE, NOTICE, and
README).

npm or PyPI distribution gets added only when each package is a **functional,
tested installer** that:

- detects the operating system and architecture,
- downloads the matching official release artifact from GitHub Releases,
- verifies its checksum against the published `checksums.txt`,
- installs the executable correctly (PATH, permissions),
- reports a clear error on unsupported platforms,
- and is covered by installation tests in CI.

Until then, the supported distribution channels are **GitHub Releases,
Homebrew, and Docker**.
