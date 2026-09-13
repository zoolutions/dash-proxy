Writing a certificate-store archive: where it may be written, what goes in it,
and what the reader will refuse. A backup that cannot restore is worse than no
backup, so every rule here fails the export rather than shipping a doubtful one.

### `export certs` falls back to an offline read only when nothing answers the socket
- **Holds because:** reading the certificate directory while a live proxy is writing it produces a torn archive. But a *live but outdated* proxy answers the socket and rejects the method, and a broad `strings.Contains(err, "can't find method")` would also swallow real export failures behind upgrade advice. The check is the exact net/rpc prefix `rpc: can't find method kamal-proxy.CertsExport`; that case fails with guidance to upgrade or stop the proxy, and never reads the directory behind its back.
- **Where:** `internal/cmd/export.go`
- **Proven by:** `TestExportCertsCommand_ExportsOffline`, `TestExportCertsCommand_EmptyStoreFails`, `TestSANCertManager_ExportStoreHoldsTheDiskLock`
- **Origin:** cubic learning bd7d0038

### The destination is validated by resolved path *and* by filesystem identity, against every existing ancestor
- **Holds because:** a string comparison of resolved paths misses aliases — a symlinked parent, a case-insensitive spelling of `acme.state`, a second mount of the same directory — and writing an archive over the live store destroys exactly what it was meant to preserve. `safeOutputPath` resolves symlinks on the destination (or on its deepest existing ancestor, rejoining the remainder) and on the store paths, compares those, and then compares `os.SameFile` identity against each existing ancestor and the certificate directory.
- **Where:** `internal/server/cert_store_export.go#safeOutputPath`, `#resolveForComparison`
- **Proven by:** `TestExportCertificateStore_RejectsOutputInsideTheStore`, `TestExportCertificateStore_RejectsSymlinkedOutputIntoTheStore`
- **Origin:** cubic learning caf46a10

### After validation the output directory is pinned as an `os.Root`, and its identity is re-checked against the certificate tree
- **Holds because:** validating a pathname and then using that pathname again is a TOCTOU: swapping a parent component between the two redirects the write. The resolved directory is opened once and that handle is reused for the staged verification read, the temp file, the rename and the directory sync. `rejectPinnedRootInsideStore` → `dirInsidePinnedTree` then walks the certificate tree *through its own pinned root* and compares directory identities against the pinned output handle, so neither side of the containment check is a re-resolvable pathname.
- **Where:** `internal/server/cert_store_export.go#writeCertArchive`, `#rejectPinnedRootInsideStore`, `#dirInsidePinnedTree`
- **Proven by:** `TestDirInsidePinnedTree`, `TestExportCertificateStore_RejectsSymlinkedOutputIntoTheStore`
- **Origin:** cubic learning caf46a10

### The containment walk fails closed: a `WalkDir` or `Info()` error aborts the export
- **Holds because:** a subtree that cannot be read or statted is a subtree whose identity was never compared. Treating it as "not a match" makes containment fail *open* — the one direction where the guard silently stops guarding. Propagating the error costs a failed export, which is recoverable.
- **Where:** `internal/server/cert_store_export.go#dirInsidePinnedTree`, `#rejectPinnedRootInsideStore`
- **Proven by:** `TestDirInsidePinnedTree`
- **Origin:** cubic learning caf46a10

### The archive is staged in a short, fixed-pattern temp file at mode 0600, verified, fsynced, renamed, and the directory fsynced
- **Holds because:** the archive carries private keys, so it must never exist world-readable, even briefly. A temp name derived from the destination basename can push past a filesystem's component-length limit for a near-limit destination and fail the export for a reason nobody will diagnose, so the pattern is short and fixed. The staged file is then read back through `readCertStoreArchive` before it is published: a backup nobody has verified is a backup nobody should trust. Directory-open and directory-sync failures fail the export — a rename that is not durable was not a successful backup — with only `ENOTSUP`/`EINVAL` excused.
- **Where:** `internal/server/cert_store_export.go#writeCertArchive`; `internal/server/cert_store_restore.go#syncOpenDir`
- **Proven by:** `TestExportCertificateStore_LongOutputBasename`, `TestExportCertificateStore_OverwritesAnExistingArchive`, `TestExportCertificateStore_ArchivesTheWholeEstate`
- **Origin:** cubic learnings 5538fcd9, 73645b0a, fab2ff92, 0ffc7900

### An unparseable `acme.state` aborts the export; a missing optional file does not
- **Holds because:** `acme.state` is the index — an archive without a usable one cannot be restored into a working estate, whatever else is in it. The optional files (`dynamic-domains.state`, per-directory account keys) are recoverable by other means, so their absence is a warning.
- **Where:** `internal/server/cert_store_export.go#collectStateEntry`, `#ExportCertificateStore`
- **Proven by:** `TestExportCertificateStore_UnparseableStateIsAnError`, `TestExportCertificateStore_OptionalFilesMayBeMissing`, `TestExportCertificateStore_SkipsInvalidOptionalFiles`
- **Origin:** cubic learning e1eecc33

### Real certificate pairs with no state file are refused; an account-key-only estate still exports
- **Holds because:** the archive reader cannot turn loose pairs into a working estate, so shipping that shape would produce a backup that fails at restore time. But ACME account keys under `certs/` are metadata, not certificates: a proxy that has registered an account and issued nothing yet has a perfectly restorable estate. The no-state check runs after filtering out both `acme_user.json` and every name `isExtraAccountKeyFile` recognises.
- **Where:** `internal/server/cert_store_export.go#ExportCertificateStore`, `#collectCertsEntries`, `#isExtraAccountKeyFile`
- **Proven by:** `TestExportCertificateStore_CertsWithoutStateIsAnError`, `TestExportCertificateStore_AccountKeyOnlyStoreExports`, `TestExportCertificateStore_AccountKeysOnlyStoreExports`
- **Origin:** cubic learnings e1eecc33, a4473d2d

### Each pair is parsed with `tls.X509KeyPair` before it is archived; an invalid one is skipped with a warning
- **Holds because:** archiving bytes that are not a usable pair produces an archive that passes its own structural check and fails at restore. Skipping with a warning is the right direction: the rest of the estate is still worth backing up, and the operator is told which certificate is not.
- **Where:** `internal/server/cert_store_export.go#collectCertPair`, `#warnMissingStateCerts`
- **Proven by:** `TestExportCertificateStore_SkipsUnparseablePairs`, `TestExportCertificateStore_Warnings`
- **Origin:** cubic learning 295ad96c

### An empty file set is an error, not an empty archive
- **Holds because:** a cron job faithfully archiving nothing every night is the failure mode that is discovered during an incident. `ErrCertStoreEmpty` is loud.
- **Where:** `internal/server/cert_store_export.go#ExportCertificateStore`
- **Proven by:** `TestExportCertificateStore_EmptyStoreIsAnError`, `TestExportCertsCommand_EmptyStoreFails`
- **Origin:** verified from the code during seeding

### Warnings from the staged verification reach the summary, minus the ones the collection pass already reported
- **Holds because:** an unusable ACME account key is only detectable by *reading* the archive, and it is exactly the kind of thing the operator needs to know at backup time rather than at restore time. Re-reporting `warnMissingCertificate` would double every line the disk-side pass already printed.
- **Where:** `internal/server/cert_store_export.go#writeCertArchive`, `CertsExportSummary`
- **Proven by:** `TestExportCertificateStore_SurfacesUnrestorableAccountKeyWarning`, `TestCertsExportSummary_RoundTrips`
- **Origin:** cubic learnings ad0a86b9, 16f47aa3

### Warnings are structured kinds internally and flattened to text only at the reporting boundary
- **Holds because:** the exporter has to filter warnings by class (drop the missing-certificate ones the collection pass already made), and matching on rendered English to do it breaks the first time a sentence is reworded.
- **Where:** `internal/server/cert_store_archive.go` (warning kinds), `internal/server/cert_store_export.go`
- **Proven by:** `TestExportCertificateStore_Warnings`, `TestVerifyCertificateArchive_DropsInvalidAccountKey`
- **Origin:** cubic learning 16f47aa3

### `export certs <output-path>` takes the path as a required positional argument
- **Holds because:** validation is `cobra.ExactArgs(1)`, so the usage line has to show the argument or the error a user gets contradicts the usage they were shown.
- **Where:** `internal/cmd/export.go`
- **Proven by:** `TestExportCertsCommand_RequiresAnOutputPath`
- **Origin:** cubic learning b8523c0b

## Related

- `cert-store-restore.md` — reading an archive back, and the importers
- `../certs/store-and-recovery.md` — the subsystem as a whole
