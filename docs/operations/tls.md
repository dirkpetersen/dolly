# TLS certificates

Both connections (to AD with `source`, and to the target with `target`) can use LDAPS or StartTLS. Dolly always verifies the server's certificate. There are two ways to make an untrusted server work:

- **`ca_file`**: a PEM file with the CA (or chain) that signed the server's certificate. Dolly adds it to the system roots. Use it for a real CA, such as an internal AD CA. Dolly then checks the chain, the expiry, and the host name as usual.
- **`tls_fingerprint`**: pin the server's own (leaf) certificate. Use it for a self-signed certificate, or for a CA you should not trust.

You cannot set `ca_file` and `tls_fingerprint` on the same side. This is a config error. Setting either on a target with a plaintext `ldap://` URL and `start_tls: false` is also a config error, because no TLS is used there.

## See what the server sends: `dolly fetch-cert`

`dolly fetch-cert` connects without verifying the server and sends no credentials. It prints each certificate of the chain (subject, issuer, validity, SHA-256 fingerprint) and a line to paste into `dolly.yaml`:

```bash
dolly fetch-cert ldaps://dc01.example.edu            # port 636
dolly fetch-cert ldap://ldap.example.edu:389         # StartTLS
dolly fetch-cert ldaps://dc01.example.edu --out ~/.config/dolly/ad-ca.pem   # also save the chain as PEM, for ca_file
```

```text
Certificate chain sent by ldap://ldap.example.edu:389 (leaf first, not verified):

  [0] subject: CN=ldap.example.org,O=Example Inc.
      issuer:  CN=docker-light-baseimage
      valid:   2018-01-01T00:00:00Z to 2028-01-01T00:00:00Z
      sha256:  sha256:3A:9F:...:C2

  [1] subject: CN=docker-light-baseimage
      issuer:  CN=docker-light-baseimage
      valid:   2015-06-01T00:00:00Z to 2025-06-01T00:00:00Z EXPIRED
      sha256:  sha256:91:0B:...:7E

To pin the server's own certificate, set in dolly.yaml (instead of ca_file):
  tls_fingerprint: ["sha256:3A:9F:...:C2"]
```

`--out` writes the chain as PEM with mode `0644`. It never overwrites an existing file. `dolly fetch-cert` reads no config file.

!!! warning "Check the fingerprint"
    A fingerprint fetched over the network is only as trustworthy as that network. Compare it with the certificate on the server itself before you use it, for example with `openssl x509 -in /path/to/cert.pem -noout -fingerprint -sha256` on that server.

## Option 1: trust a CA with `ca_file`

Run `dolly fetch-cert URL --out FILE`, check that the CA in the chain is the one you expect, and point the config at the file:

```yaml
source:
  ca_file: ad-ca.pem   # resolved against the config file's directory if relative
```

or, for the target:

```yaml
target:
  ca_file: ldap-ca.pem
```

## Option 2: pin the certificate with `tls_fingerprint`

`tls_fingerprint` is a list of SHA-256 fingerprints of the certificate's DER bytes. Write each as `sha256:AB:CD:...`, or as plain hex, with or without colons, in any case.

```yaml
source:
  tls_fingerprint:
    - "sha256:3A:9F:...:C2"   # dc01
    - "sha256:55:1C:...:08"   # dc02

target:
  tls_fingerprint: ["sha256:7D:42:...:19"]
```

- Dolly accepts the connection only if the leaf certificate matches one entry.
- The CA chain, the expiry, and the host name are not checked, because the pin itself identifies the server.
- Each AD DC has its own certificate. List one entry per DC. Any DC may match any entry.
- When a certificate is renewed, add the new fingerprint before the change, and remove the old one after it.
- A pin suits the demo certificate of the `osixia/openldap` image. Its CA has expired, and its CA private key is public, so you should not trust that CA.

## When a handshake fails

If certificate verification fails, in `dolly sync` and in `dolly check`, the error names the cause, the server's leaf fingerprint, and the `dolly fetch-cert URL` command to run. A pinned server whose certificate does not match says so and prints the fingerprint it sent.

## `openssl` as an alternative

You can also use `openssl`:

```bash
openssl s_client -connect dc01.example.edu:636 -showcerts </dev/null 2>/dev/null \
  | awk '/BEGIN CERTIFICATE/,/END CERTIFICATE/' > ~/.config/dolly/ad-ca.pem
```

For StartTLS on port 389, add `-starttls ldap`. Show the fingerprint with:

```bash
openssl x509 -in ~/.config/dolly/ad-ca.pem -noout -subject -issuer -fingerprint -sha256
```

!!! warning "Never skip verification"
    Dolly never disables certificate verification. Without a pin, it checks the chain against the system roots plus `ca_file`. With a pin, it checks the pin. There is no option to skip verification. If the handshake still fails, check the fingerprint and the file's contents. Do not look for a bypass.
