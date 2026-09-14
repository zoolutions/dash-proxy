# Certificate store: export, verify, restore, import

The certificate estate is four things in the data directory: `acme.state` (the
index), `certs/` (one directory per certificate plus the ACME account keys),
`dynamic-domains.state`, and nothing else. `CertStorePaths`
(`internal/server/cert_store_export.go`) names them;
`Config.CertStorePaths()` builds it.

The archive mirrors that layout exactly, so a restore is a faithful extraction:

```
acme.state
dynamic-domains.state
certs/acme_user.json                    primary ACME account key
certs/acme_user_staging.json            per-directory account keys (--tls-staging)
certs/acme_user_<8 lowercase hex>.json
certs/<sanitized cert id>/cert.pem
certs/<sanitized cert id>/key.pem
```

`isExtraAccountKeyFile` is the **closed** predicate for the per-directory
names — `acme_user_staging.json`, or `acme_user_` + exactly 8 lowercase hex
digits + `.json` — shared by the exporter and the archive reader so nothing is
exported that a restore would refuse, and no arbitrary `acme_user_*.json` is
adopted as an ACME identity.

## Export

`CommandHandler.CertsExport` → `SANCertManager.ExportStore` takes `stateMu`, the
store's disk-write lock, so a backup taken mid-renewal is never torn; with no
certificate manager the store has no writers and `ExportCertificateStore` runs
directly. `dash-proxy export certs <path>` dials the socket first and falls back
to an offline read only when **nothing answers** — an RPC error whose text
starts with `rpc: can't find method kamal-proxy.CertsExport` means a live but
outdated proxy, and that case fails with upgrade guidance rather than reading a
directory something is writing.

`ExportCertificateStore` in order:

1. `safeOutputPath` resolves symlinks on the destination and on the store paths,
   compares resolved strings, and then compares filesystem identity
   (`os.SameFile`) against each existing ancestor — an alias or a
   case-insensitive spelling of `acme.state` is refused too.
2. `collectStateEntry` reads `acme.state`. It is the one file whose unparseable
   presence **aborts** the export: a backup without a usable index cannot
   restore. `validateManagerState` runs here.
3. `collectCertsEntries` walks `certs/`, capturing the account keys and every
   complete pair. `collectCertPair` parses each pair with `tls.X509KeyPair` and
   skips an invalid one with a warning rather than archiving it.
   `warnMissingStateCerts` flags certificates the state names but disk lacks.
4. Certificates present with no state file is refused. Account keys — primary
   and per-directory — do not count as certificates, so an account-only estate
   (registered, nothing issued yet) still exports.
5. An empty file set is `ErrCertStoreEmpty`: a cron job faithfully archiving
   nothing is worse than a loud failure.
6. `writeCertArchive` pins the output directory as an `os.Root`,
   re-validates that handle's identity against the certificate tree
   (`rejectPinnedRootInsideStore` → `dirInsidePinnedTree`, walking the tree
   through its own pinned root so neither side of the comparison is a
   re-resolvable pathname), creates a short fixed-pattern temp file with
   `O_EXCL` and mode 0600, writes, **reads the staged archive back through
   `readCertStoreArchive`**, fsyncs, renames, and fsyncs the directory. A
   verification failure removes the temp file. Directory-open and
   directory-sync failures fail the export, except `ENOTSUP`/`EINVAL`.
7. Warnings the staged verification produced are merged into the summary,
   except `warnMissingCertificate` ones the collection pass already reported.

## Reading an archive

`readCertStoreArchive` (`internal/server/cert_store_archive.go`) is shared by
verify, restore and the exporter's self-check. Structural problems are errors —
a backup that fails here cannot be trusted — while an expired certificate is
not: a faithful backup of an expired certificate is still a backup.

Bounds: `maxCertArchiveBytes` (512 MiB decompressed), `maxCertArchiveEntries`
(100 000 headers), and `maxCertArchiveHeaderBytes` (64 MiB) for the PAX/GNU
metadata `tar.Reader.Next()` consumes before the entry counter can run. The
whole decompressed gzip stream goes through `cappedReader`, whose boundary
semantics are exact: a zero-length read returns `(0, nil)` without touching the
underlying reader; at an exhausted budget it probes one byte, treating `io.EOF`
as "ended exactly at the cap" and any byte as overflow. After tar EOF the stream
is drained to its own EOF so the gzip trailer and checksum are validated.

`placeEntry` routes each entry and refuses any name the exporter would never
write, including a certificate directory that is not already in
`sanitizeFilename` form — two spellings collapsing to one on-disk path is how a
restore overwrites the wrong certificate.

`validateManagerState` requires: both maps present, no null record, every
identifier equal to its map key, every sanitized identifier safe as a directory
name and unique after sanitization, and every `DomainMap` key covered by its
target's `Domains` under the same wildcard-aware `identifiersCover` the issuance
path uses. The reader additionally cross-checks each state-referenced pair's
leaf DNS names (order-independent set equality) and expiry (second precision)
against the state record.

The ACME account payload is validated against the boot-time loader's contract:
only an `*ecdsa.PrivateKey` is usable. A missing, invalid or non-ECDSA key is
**not** fatal — the account is omitted with a `warnAccountKey` warning saying
the next boot will register a fresh one.

## Restore and verify

Both run offline against a stopped proxy: `dash-proxy import certs --archive
<path>` (add `--force` to overwrite a non-empty store, `--verify` to report
without touching it). `RestoreCertificateStore` writes certificate pairs, then
the account keys, then `dynamic-domains.state`, and `acme.state` **last**, so an
interrupted restore never leaves an index naming files that are not there.
Every write goes through `writeFileStaged`: a unique same-directory temp file,
mode 0600, fsync, rename, and removal on every failure path.

`removeStaleCertDirs` runs **after** the state commit, deliberately: a restore
that fails midway then leaves the old store intact rather than an old index
pointing at deleted directories. It counts a removal only when `Lstat` confirms
the directory exists, fails the restore on any inspection error other than
`os.ErrNotExist`, and syncs `certsPath` only when at least one real unlink
happened.

Recovery is certificates **only**. The routing table is a separate file: restore
`dash-proxy.state` (or its `.bak`) while the proxy is stopped, before starting
it; without that, redeploy the TLS services after boot to rebuild routing.
`dash-proxy domains list` shows dynamic `--tls-domains-source` domains only, so
it is not evidence that static `--tls --host` services came back — verify those
with a TLS handshake and the expiry metrics.

## Importers

- **Traefik** (`internal/server/traefik_import.go`) —
  `import certs --traefik-acme <acme.json> [--resolver <name>]`, offline.
  Certificates are accepted on their validity window alone; the
  registered-versus-dynamic policy stays in `SANCertManager.GetCertificate`.
  `loadStateForImport` decodes existing state into a zero-value `managerState`
  and refuses nil required maps, leaving an invalid state file untouched.
  `cert.pem` and `key.pem` are replaced per file through temp+rename — an
  interrupted pair is caught by `loadState`'s pair check and falls through to
  ordinary provisioning, so no staged directory swap is needed.
- **Legacy HTTP-01 cache** (`internal/server/san_cert_import.go`) —
  `importLegacyHTTP01Cache` runs from `Initialize` with **no** manager or store
  lock held, because adoption takes its own.

`import certs` creates a non-empty `--data-dir` before importing, so an empty or
wholly skipped import still writes `acme.state`. Its flag groups: one of
`--traefik-acme`/`--archive` is required, those two are mutually exclusive,
`--archive` excludes `--resolver`, and `--verify`/`--force`/`--traefik-acme` are
mutually exclusive — which is what leaves `--archive --verify` and
`--archive --force` the only valid combinations of those three.

## Related

- `summary.md` — issuance, batching, renewal
- `../review/cert-store-export.md`, `../review/cert-store-restore.md`
