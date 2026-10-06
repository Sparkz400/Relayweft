---
title: Contributing
weight: 5
---

<!-- include CONTRIBUTING.md optional -->

<!-- if-missing CONTRIBUTING.md -->
<!-- include README.md#development body -->
<!-- end -->

## The docs site

The site is built from the repository's own Markdown, so a fix to the README or to `docs/` is a fix to the site too. `website/pages` holds the site's page list and the few pages of its own; `website/gen` pulls in the rest and checks every link. To preview it, with Go and [Hugo](https://gohugo.io/installation/) (extended, the version in `.github/workflows/pages.yml`):

```sh
website/build.sh serve    # http://localhost:1313/Relayweft/
```

`website/build.sh` alone builds the site into `_site`. The `pages` workflow does the same on every pull request that touches the docs, and publishes the site from `main`.

The demo GIF is recorded with [vhs](https://github.com/charmbracelet/vhs) from `docs/demo/demo.tape`: run the `demo` workflow (or `docs/demo/record.sh` on Linux or macOS) and commit `docs/demo/demo.gif` and `demo.webm`.
