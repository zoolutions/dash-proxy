Reading an archive back, and the two importers. Structural doubt is an error
here; an expired certificate is not — a faithful backup of an expired
certificate is still a faithful backup.

### The whole decompressed gzip stream is capped, not just the file entries
- **Holds because:** `tar.Reader.Next()` consumes PAX/GNU metadata records before any entry counter can see them, so a cap applied only to entry bodies leaves an unbounded work primitive in the header path. `maxCertArchiveBytes` (512 MiB), `maxCertArchiveEntries` (100 000 headers) and `maxCertArchiveHeaderBytes` (64 MiB) are all enforced, with the capped reader wrapping the gzip stream that feeds the tar reader.
- **Where:** `internal/server/cert_store_archive.go` (`maxCertArchiveBytes` 29, `maxCertArchiveEntries` 30, `maxCertArchiveHeaderBytes` 37), `#readCertStoreArchive`
- **Proven by:** `TestCappedReader_FailsBeyondTheLimit`, `TestCappedReader_ExactlyAtTheLimitIsNotOversized`
- **Origin:** cubic learning 1cedc5a5

### `cappedReader.Read`'s boundary semantics are exact, and a zero-length read never touches the underlying reader
- **Holds because:** an archive ending *exactly* at the cap is legal and must not be rejected, while one byte past it must be. At an exhausted budget the reader probes a single byte: `io.EOF` means it ended at the cap and is accepted; any positive count is overflow and fails with `errCertArchiveTooLarge`; a legal `(0, nil)` is passed through rather than read as overflow. A zero-length read returns `(0, nil)` at the top of `Read` regardless of budget, because `io.Reader` permits it and calling through would misreport the boundary.
- **Where:** `internal/server/cert_store_archive.go#cappedReader`
- **Proven by:** `TestCappedReader_ExactlyAtTheLimitIsNotOversized`, `TestCappedReader_FailsBeyondTheLimit`
- **Origin:** cubic learning 1cedc5a5

### After tar EOF the stream is drained to its own EOF, so the gzip trailer is validated
- **Holds because:** tar EOF is two zero blocks — it says nothing about whether the gzip stream that carried them is intact. Stopping there accepts an archive with a corrupt trailer or a failing CRC, and leaves any trailing decompressed data uncounted against the cap.
- **Where:** `internal/server/cert_store_archive.go#readCertStoreArchive`
- **Proven by:** `TestVerifyCertificateArchive_RejectsCorruptGzipTrailer`
- **Origin:** cubic learning 1cedc5a5

### Emptiness is counted in regular files, while the DoS cap counts every header
- **Holds because:** an archive of nothing but directory and metadata headers restores into an empty store, so it must be refused as empty — but those same headers are work, so they still count toward the entry cap. The two counters answer different questions.
- **Where:** `internal/server/cert_store_archive.go#readCertStoreArchive`
- **Proven by:** `TestVerifyCertificateArchive_DirectoryOnlyArchiveIsEmpty`
- **Origin:** cubic learning 1cedc5a5

### `placeEntry` refuses any name the exporter would never write, including an unsanitized certificate directory
- **Holds because:** two spellings that collapse to one on-disk path is how a restore overwrites the wrong certificate. The requirement is `dir == sanitizeFilename(dir)` — already in sanitized form, not sanitizable-to — so no alternate spelling is normalised into an existing directory. Symlink entries are refused outright.
- **Where:** `internal/server/cert_store_archive.go#placeEntry`
- **Proven by:** `TestReadCertStoreArchive_RejectsUnsanitizedDirNames`, `TestVerifyCertificateArchive_RejectsSymlinkEntries`
- **Origin:** cubic learnings 4d3f5498, cc2f2df4

### `validateManagerState` rejects identifier/key mismatches and sanitization collisions before anything is mapped to a pair
- **Holds because:** a record whose `Identifier` differs from its map key, or two distinct identifiers that sanitize to the same directory, produce certificates that fail renewal, removal and host coverage later — long after the archive was declared good. It also requires both maps present, no null record, and every `DomainMap` key covered by its target's `Domains` under the same wildcard-aware `identifiersCover` the issuance path uses, so the archive cannot disagree with issuance about what covers what.
- **Where:** `internal/server/cert_store_archive.go#validateManagerState`
- **Proven by:** `TestVerifyCertificateArchive_RejectsCorruptStateRecords`, `TestExportCertificateStore_InvalidStateIsAnError`
- **Origin:** cubic learnings a21f3e92, 4e3e8493

### Each state-referenced pair's leaf is cross-checked against its state record: DNS names as an exact set, expiry to the second
- **Holds because:** state that has drifted from the leaf is a store that will renew the wrong identifier set or refuse a host it is serving. The set equality is order-independent and exact in both directions — a leaf covering *more* than the record claims is as wrong as one covering less.
- **Where:** `internal/server/cert_store_archive.go#readCertStoreArchive`
- **Proven by:** `TestVerifyCertificateArchive_RejectsLeafDisagreeingWithState`, `TestVerifyCertificateArchive_ReportsDomainsAndExpiries`
- **Origin:** cubic learning 312e2ba2

### An unusable ACME account key is a warning, not a failure — and only `*ecdsa.PrivateKey` is usable
- **Holds because:** the boot-time loader accepts nothing else, so a valid RSA key would restore and then fail to load, which is worse than not restoring it. Losing the account costs a fresh registration on the next boot; losing the certificates would cost an outage. The warning says exactly that.
- **Where:** `internal/server/cert_store_archive.go` (account payload validation, `warnAccountKey`)
- **Proven by:** `TestVerifyCertificateArchive_DropsInvalidAccountKey`
- **Origin:** cubic learning d9a06e31

### A restore writes `acme.state` last, and every write goes through `writeFileStaged`
- **Holds because:** the index must never name files that are not on disk, so certificate pairs, then account keys, then `dynamic-domains.state`, then the index. `writeFileStaged` creates a unique same-directory temp at mode 0600, writes, fsyncs, closes, renames, syncs the directory, and removes the temp on every failure path — so a forced restore cannot follow a planted symlink, cannot inherit a pre-existing temp file's permissions for a private key, and cannot leave a truncated file or stray temp behind. The temp pattern is short and fixed for the same length reason as the exporter's.
- **Where:** `internal/server/cert_store_restore.go#RestoreCertificateStore`, `#writeFileStaged`, `#syncOpenDir`
- **Proven by:** `TestRestoreCertificateStore_RoundTrip`, `TestRestoreCertificateStore_RefusesNonEmptyStore`, `TestRestoreCertificateStore_RoundTripsExtraAccountKeysAndDirectories`
- **Origin:** cubic learnings 4688cd49, 18bcaeab, 5538fcd9

### Stale certificate directories are removed *after* the state commit, not before
- **Holds because:** a restore that fails midway must leave the old store intact. Removing first and failing later leaves an old index pointing at deleted directories — unrecoverable without the archive that just failed to apply. A post-commit removal failure is surfaced as an error against an already-restored state, which is the recoverable side of the trade and needs no quarantine-and-rollback machinery.
- **Where:** `internal/server/cert_store_restore.go#RestoreCertificateStore`, `#removeStaleCertDirs`
- **Proven by:** `TestRestoreCertificateStore_ForceRemovesStaleStateReferencedDirs`, `TestRestoreCertificateStore_ForceKeepsUnreferencedCertDirs`
- **Origin:** cubic learning 766ad149; PR #95 review thread

### A removal counts only when `Lstat` confirms the directory existed, and `certsPath` is synced only if a real unlink happened
- **Holds because:** syncing a directory that was never changed — or that does not exist — either fails the restore after the state was committed or reports durability that was never at stake. Any inspection error other than `os.ErrNotExist` fails the restore: a path that cannot be statted is a path whose removal was never verified. The degenerate shape where state references a certificate whose directory is absent stays tolerated, succeeding with only the re-order warning. Directory-sync failures on a real removal remain fatal, or a crash resurrects the stale directories.
- **Where:** `internal/server/cert_store_restore.go#removeStaleCertDirs`
- **Proven by:** `TestRestoreCertificateStore_DegenerateArchiveIntoFreshStore`, `TestRestoreCertificateStore_ForceRemovesStaleStateReferencedDirs`
- **Origin:** cubic learnings 702a3400, 22f3ec11

### Directory-sync errno policy lives in exactly one place
- **Holds because:** `ENOTSUP`/`EINVAL` are the only excusable outcomes (some filesystems do not support directory fsync), and every other errno must fail. Spreading that judgment across the export path, the restore path and the pinned-root path guarantees the copy someone forgets is the one that silently swallows a real I/O error. `syncOpenDir(*os.File)` holds it; `syncDir` (pathname) and `syncRootDir` (pinned `os.Root`) delegate.
- **Where:** `internal/server/cert_store_restore.go#syncOpenDir` (241-246) and `#syncDir` (227-235); `internal/server/cert_store_export.go#syncRootDir` (628)
- **Proven by:** no direct test — the policy is one three-line function, asserted indirectly by every restore and export round-trip
- **Origin:** cubic learning b969eeb3

### Recovery is certificates only, so the runbook restores routing state separately, before startup
- **Holds because:** the archive holds `acme.state`, `certs/` and `dynamic-domains.state` — not the routing table. A restore that stops there boots a proxy with certificates and no services. `dash-proxy.state` (or its `.bak`) is restored while the proxy is stopped; without it, redeploy the TLS services after boot. And `domains list` reports only dynamic `--tls-domains-source` domains, so it is not evidence that static `--tls --host` services came back — verify those with a TLS handshake and the expiry metrics.
- **Where:** `lode/certs/store-and-recovery.md`; `internal/server/cert_store_export.go#CertStorePaths`, `internal/cmd/domains.go`
- **Proven by:** documentation rule, not code; the paths are asserted by `TestRestoreCertificateStore_RoundTrip`
- **Origin:** cubic learnings 4a7924d7, 192f9ab3

### `import certs` flag modes: `--verify`, `--force` and `--traefik-acme` are mutually exclusive
- **Holds because:** verifying and forcing are opposite intents, and the Traefik importer reads a completely different input. One of `--traefik-acme`/`--archive` is required and those two are exclusive; `--archive` excludes `--resolver`. What survives is `--archive --verify` and `--archive --force` as the only valid combinations of those three.
- **Where:** `internal/cmd/import.go`
- **Proven by:** `TestImportCertsCommand_FlagValidation`, `TestImportCertsCommand_RequiresTraefikAcmeFlag`, `TestImportCertsCommand_VerifyReportsWithoutWriting`
- **Origin:** cubic learning 0b344bc1

### `import certs` creates a non-empty `--data-dir` before importing
- **Holds because:** an import where every entry is skipped still has to write `acme.state`, and a missing directory turns that into a confusing write failure at the end of an otherwise successful run.
- **Where:** `internal/cmd/import.go`, `internal/cmd/util.go#ensureDataDir`
- **Proven by:** `TestImportCertsCommand_CreatesAMissingDataDir`, `TestImportCertsCommand_ImportsIntoTheDataDir`
- **Origin:** cubic learning e71772eb

### The Traefik importer accepts certificates on their validity window alone, and leaves policy to `GetCertificate`
- **Holds because:** registered-versus-dynamic is a live routing question the importer cannot answer offline — it has no router. Deciding it at import time would bake a snapshot of the deployment into the store. `loadStateForImport` decodes existing state into a zero-value `managerState` and refuses nil required maps, so an invalid state file is left untouched rather than half-overwritten.
- **Where:** `internal/server/traefik_import.go#loadStateForImport`; `internal/server/san_cert_manager.go#GetCertificate`
- **Proven by:** `TestImportTraefikCertificates_StateLoadsIntoSANCertManager`, `TestImportTraefikCertificates_SkipsExpiredAndNotYetValid`, `TestImportTraefikCertificates_ImportsValidEntries`
- **Origin:** cubic learnings 8f4b1ef2, 68ca61cb

### Not a bug: the Traefik importer replaces `cert.pem` and `key.pem` per file, with no pair-level transaction
- **Holds because:** both files are staged as `.tmp` before either rename, so a failed or partial *write* cannot break an existing pair. What remains is a process kill exactly between the two renames, and that already degrades gracefully: `loadState`'s `tls.LoadX509KeyPair` fails, the certificate loads with a nil `tls.Certificate`, and `GetCertificate` falls through to ordinary provisioning — a re-order, not certificate loss. A pair-level atomic swap is not implementable in the manager's fixed layout (POSIX `rename` cannot atomically replace a non-empty directory, and the running manager reads these exact paths), and the import is offline and idempotent.
- **Where:** `internal/server/traefik_import.go`
- **Proven by:** `TestImportTraefikCertificates_ReplacesShorterLivedExistingMapping`, `TestImportTraefikCertificates_KeepsLongerLivedExistingMapping`
- **Origin:** cubic learning bdc8c1f9; PR #91 review thread (partially accepted, remainder declined with reasoning)

## Related

- `cert-store-export.md` — writing an archive
- `../certs/store-and-recovery.md` — the subsystem as a whole
