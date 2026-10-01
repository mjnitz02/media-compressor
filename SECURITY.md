# Security Policy

## Supported versions

This is a personal open-source project. Only the latest release on `main`
receives security fixes.

## Reporting a vulnerability

Please **do not open a public issue** for security problems.

Report privately via GitHub's
[private vulnerability reporting](https://github.com/mjnitz02/media-compressor/security/advisories/new),
which notifies the maintainer directly and keeps the report confidential until
a fix ships.

Expect an initial response within roughly a week. As a single-maintainer
project there is no formal SLA, but reports are taken seriously and credited
in the advisory unless you'd rather stay anonymous.

## Scope

In scope: this repository's source, its GitHub Actions workflows, and the
artifacts it publishes — the `ghcr.io/mjnitz02/media-compressor` image and the
binary attached to each release.

Worth knowing about this one specifically: the program replaces media files in
place and deletes the original after a verified encode. A report that shows a
path to data loss — a verification that can be fooled, a replace that can be
made to write outside its library roots — is a security report as far as this
project is concerned, and is the most valuable kind.

Out of scope: vulnerabilities in third-party dependencies (report those
upstream; Dependabot tracks them here), vulnerabilities in ffmpeg itself, and
issues that require an already compromised local machine.
