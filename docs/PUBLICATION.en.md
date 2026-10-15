[简体中文](PUBLICATION.md) | [English](PUBLICATION.en.md)

# Public development preview snapshot

Public repository: <https://github.com/solarise94/cloud2api-public>.

This public release establishes a new Git repository from the reviewed snapshot of the current files. The initial `main` has only a single root commit with no parent commit, and it does not import the original development repository's Git objects, commits, branches, or tags. Subsequent public maintenance can continue committing on this new starting point; this does not mean the project can never keep a public development history later.

## Release scope

Included:

- cloud2api source code, static pages, and automated tests that use synthetic data.
- Example configuration, build, and deployment scripts without actual credentials.
- Current usage, interface, and contribution documentation.
- The OpenList derived patch, pinned upstream version information, and the AGPL-3.0 license text.

Not included:

- The original repository's `.git`, old commit author emails, historical branches, and tags.
- The actual `config.yaml`, environment files, drive content, runtime directories, and password files.
- Databases, logs, backups, container inspection results, and local deployment tooling.
- Internal design timelines, implementation plans, and historical upgrade notes.
- Local mirrors, container volumes, build caches, or real-account test attachments.

The new root commit uses a public GitHub username and a `noreply` email, and does not reuse the private commit email from the development repository. Release attachments are generated with `git archive` from the public root commit, not packaged directly from a working directory that contains runtime data.

## Privacy checks and review

Release files are exported using an explicit file manifest, not by copying entire directories. During export, symbolic links, path escapes, runtime data directories, and sensitive file types are rejected; the final file contents, the Git tree, and the commit metadata are then checked. Fixed strings in examples and tests should be used only for synthetic fixtures, and must not be replaced with your own credentials and then committed.

This time, Gitleaks 8.24.3 with its default rules was used to scan the export directory; the raw hits were 8 synthetic test values and 1 manually public Baidu login application parameter that upstream publishes on purpose. After checking each item, no actual deployment credentials were found. The public parameter has been compared against the [OpenList-APIPages source](https://github.com/OpenListTeam/OpenList-APIPages/blob/581b57aa/frontend/src/pages/TokenPage.tsx#L118-L124) and is not mistaken for a local account secret. `.gitleaksignore` records only these specific files, rules, and line numbers, and does not exclude an entire test directory or all credential rules.

You can verify this at the repository root:

```sh
gitleaks dir . --redact=100 --gitleaks-ignore-path .gitleaksignore
```

The image provenance credential for the Logo is retained; it describes the generation source and does not contain drive account credentials.

Automated scanning and manual review can only reduce risk; they cannot guarantee that no unknown sensitive information exists. If you find a suspected credential, do not paste the original text into a public Issue; first revoke or rotate the corresponding credential, and only then deal with the public content and its caches.

## Verification boundaries

The Go and frontend automated tests use isolated fixtures and do not perform uploads, deletions, or fault injection on real drives. Passing the simulated tests does not mean that all drive account types, long-term renewal, and failure recovery have been verified; for the specific platform scope, see [the platform matrix](../openlist-patch/docs/PLATFORMS.en.md).

The clipboard compatibility fix has automated regression coverage, but an independent end-to-end acceptance test of the system clipboard in Chrome has not yet been completed, so a "Copied" label on the button must not be regarded as independent verification of the system clipboard.

The current public content is a development preview, not a statement of complete production hardening or commercial certification. The cloud2api core has not yet been assigned a separate license; being publicly visible does not mean permission to reuse or redistribute is granted. The OpenList derived parts are handled under their AGPL-3.0 declaration; see [the OpenList license text](../openlist-patch/LICENSE) for details.
