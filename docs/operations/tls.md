# TLS certificates

Both connections — to AD (`source`) and to the target (`target`) — can use LDAPS or StartTLS, and both accept an optional `ca_file`.

## When the handshake fails

If a TLS handshake fails because the server's CA isn't trusted by the machine running Dolly, that's usually an internal AD certificate authority (or an internal CA for the target) that isn't in the system trust store. Fetch the server's certificate chain and point `ca_file` at it:

```bash
openssl s_client -connect dc01.example.edu:636 -showcerts </dev/null 2>/dev/null \
  | awk '/BEGIN CERTIFICATE/,/END CERTIFICATE/' > ~/.config/dolly/ad-ca.pem
```

For StartTLS on port 389 instead of LDAPS on 636, add `-starttls ldap`:

```bash
openssl s_client -connect ldap.example.edu:389 -starttls ldap -showcerts </dev/null 2>/dev/null \
  | awk '/BEGIN CERTIFICATE/,/END CERTIFICATE/' > ~/.config/dolly/ldap-ca.pem
```

## Verify the fingerprint

Always check the fingerprint against a trusted source (your PKI team, a certificate already deployed elsewhere) before relying on a fetched chain:

```bash
openssl x509 -in ~/.config/dolly/ad-ca.pem -noout -subject -issuer -fingerprint -sha256
```

## Point the config at it

```yaml
source:
  ca_file: ad-ca.pem   # resolved against the config file's directory if relative
```

or, for the target:

```yaml
target:
  ca_file: ldap-ca.pem
```

!!! warning "Never skip verification"
    Dolly loads `ca_file` into the TLS root pool and validates against it. There is no option to skip verification, and Dolly does not fall back to disabling certificate verification. If the handshake still fails after setting `ca_file`, re-check the fingerprint and the file's contents rather than looking for a bypass.
