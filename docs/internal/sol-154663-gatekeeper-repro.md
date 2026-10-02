# SOL-154663: macOS Gatekeeper block on the released binary

Reproduction and workaround findings for SOL-154663 (epic SOL-153075). The
reporter saw "Apple could not verify 'solace-broker-mcp' is free of malware
that may harm your Mac."

## Environment

| Item | Value |
|---|---|
| Release | v0.10.0 (2026-09-30), `solace-broker-mcp-v0.10.0-darwin-arm64.tar.gz` |
| macOS | 26.7.1, arm64 |
| Browser path | Chrome download, then `tar xzf` |
| Non-browser path | `gh release download -R SolaceProducts/solace-broker-mcp` |

How the reporter obtained the binary (browser, AirDrop, curl), their macOS
version and architecture, and whether they used the right archive are
unconfirmed.

## Reproduction

1. Download the arm64 archive in Chrome. It carries
   `com.apple.quarantine: 0081;…;Chrome;…`.
2. `tar xzf` the archive. The extracted binary inherits the same quarantine
   value.
3. Double-click `solace-broker-mcp` in Finder. The dialog reads
   **"solace-broker-mcp" Not Opened**, with the reporter's wording, and offers
   only **Move to Trash** and **Done**.

### Download paths tried

All with `tar xzf` extraction, v0.10.0 darwin-arm64, macOS 26.7.1 arm64.

| Download method | Quarantine on extracted binary | Runs |
|---|---|---|
| Chrome | Yes (`0081;…;Chrome;…`) | No, "Not Opened" dialog |
| `curl -LO` | No | Yes, prints `v0.10.0` |
| `gh release download` | No | Yes |

### Not tested

These ticket candidates were not run, and their outcome is unknown:

- **Intel (amd64) archive and Intel hardware.** Only arm64 was available. The
  quarantine mechanism is architecture-independent, but the amd64 binary was
  not run.
- **Fresh user account.** Quarantine is per-file, so a clean account adds
  nothing over a fresh download; not done.
- **AirDrop and Slack as the delivery path.** Not reproduced. Both can apply
  quarantine, but this was not checked.
- **Other macOS versions.** Only 26.7.1. Right-click → Open may behave
  differently on earlier releases.

Still open from the reporter: macOS version, architecture, release and asset
filename, how the archive was obtained, and the exact commands run.

## Root cause

The binary is unsigned for distribution: `codesign -dv` reports
`Signature=adhoc`, `Identifier=a.out`, `TeamIdentifier=not set`, and
`spctl -a -t exec` reports `rejected`. That alone does not block it. The
`gh release download` copy, with the same signature state and no quarantine
attribute, ran normally. The trigger is the quarantine attribute added by the
browser; the missing Developer ID signature and notarization is why Gatekeeper
then offers no way to open it from the dialog.

The non-browser copy also passed `shasum -c` and `gh attestation verify`
(exit 0), so the archive is the genuine release artifact.

## Workarounds

Each was tried on a separate `ditto` copy of the quarantined extract.

| # | Workaround | Result |
|---|---|---|
| 1 | `xattr -d com.apple.quarantine solace-broker-mcp` | Works. Finder opens the binary with no dialog. `spctl` still reports `rejected`. |
| 2 | Right-click → Open | Fails. Same "Not Opened" dialog, no Open button. |
| 3 | System Settings → Privacy & Security → Open Anyway | Works. A second "Open "solace-broker-mcp"?" dialog offers **Open Anyway**; a password or Touch ID prompt follows and the binary runs. The quarantine attribute stays, its flags field changing from `0081` to `00c1`. |

## Reasoning

Why each result came out as it did, and why the docs say what they say.

- **Quarantine is the trigger, not the signature.** Gatekeeper assesses a
  file only when it carries `com.apple.quarantine`. Browsers set it on
  download and `tar xzf` copies it onto the extracted binary. The
  `gh release download` copy has the same ad-hoc signature but no attribute,
  so nothing is assessed and it runs. That isolates the attribute as the
  cause.
- **The signature decides what the dialog offers.** With no Developer ID and
  no notarization the assessment fails and the dialog has no Open button.
  This is also why right-click → Open fails on macOS 26: it used to be the
  documented override for unidentified developers, and here it shows the same
  dialog.
- **`xattr -d` works by skipping the check.** Removing the attribute means
  Gatekeeper never assesses the file. The binary is unchanged, which is why
  `spctl` still reports `rejected`: it reports what an assessment would
  conclude, not what launch enforces.
- **Open Anyway works by recording approval.** The attribute stays; the flags
  change from `0081` to `00c1` once the user has approved that file.
- **Verify first.** Both workarounds switch off or satisfy a safety check, so
  the user guide requires `shasum -c` and `gh attestation verify` before
  either. The attestation command carries `--signer-workflow` and the exact
  archive filename, as the README explains: `--repo` alone accepts an
  attestation from any workflow in the repository, and the command takes one
  file, not a glob. The command is inlined in the user guide so support can
  copy it without following the link.
- **Docs-only is the scope.** The ticket asks for a troubleshooting entry that
  quotes the dialog wording, sits under a linkable anchor, and requires
  verification before any override. Right-click → Open is deliberately not
  recommended, since it fails.
- **Signing is the real fix, kept separate.** Only Developer ID signing plus
  notarization removes the dialog. It needs Apple credentials as secrets and
  a change to `release.yml` (sign before the archive, checksum and
  attestation steps so the digests match the shipped file). Bare Mach-O
  binaries cannot be stapled, so notarization is checked online on first run.

## Follow-up

Developer ID signing and notarization of the macOS binaries is out of scope
here and is the fix that removes the dialog. A follow-up ticket is still to be raised.
