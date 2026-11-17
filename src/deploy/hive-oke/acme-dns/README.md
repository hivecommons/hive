# acme-dns: delegated RFC 2136 zone for wildcard DNS-01

hivecommons.dev DNS lives at DNSimple (CNCF/LFX), which cannot issue a
single-domain token. cert-manager instead writes challenges, with TSIG-signed
RFC 2136 updates, into the subzone `acme.hive.hivecommons.dev` served here.
LFX delegates once with fixed records:

| Type  | Name                                   | Value                              |
|-------|----------------------------------------|------------------------------------|
| A     | `ns-acme.hive.hivecommons.dev`         | `129.80.107.104`                   |
| NS    | `acme.hive.hivecommons.dev`            | `ns-acme.hive.hivecommons.dev`     |
| CNAME | `_acme-challenge.hive.hivecommons.dev` | `hive.acme.hive.hivecommons.dev`   |
| CNAME | `_acme-challenge.lke.hive.hivecommons.dev` | `lke-hive.acme.hive.hivecommons.dev` |
| CNAME | `_acme-challenge.dibs.hivecommons.dev` | `dibs.acme.hive.hivecommons.dev`   |

Knot only accepts TXT updates for `hive`, `lke-hive` and `dibs` under the zone,
signed with the `acme-dns-tsig` key, and does not recurse.

## Rollout

1. Create both Secrets (commands in `secret.example.yaml`), then
   `kubectl apply -k .`. Confirm the Service holds `129.80.107.104`.
2. Ask LFX to add the records above. Check:
   `dig @129.80.107.104 SOA acme.hive.hivecommons.dev` and
   `dig NS acme.hive.hivecommons.dev`.
3. `kubectl apply -f clusterissuer-rfc2136.yaml` (the Cloudflare issuer stays).
4. Staging test: a throwaway Certificate for `acme-test.hive.hivecommons.dev`
   against a copy of the issuer using the Let's Encrypt staging URL. It must
   reach `Ready`.
5. Switch `hive-hub/hive-wildcard-tls` to `letsencrypt-dns01-rfc2136` and
   force-renew well before 2027-01-08 (`cmctl renew -n hive-hub hive-wildcard-tls`).
   Confirm the mail receiver's synced copy picked up the new certificate.
6. Remove the Cloudflare solver and its token Secret.

The zonefile is seeded into an `emptyDir`: challenge records are short-lived,
so a pod restart loses nothing that cert-manager does not re-create.
