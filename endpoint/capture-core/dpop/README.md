# dpop — the device side of the compact-JWS contract

RFC 7515 ES256 compact JWS, RFC 9449 DPoP proof, RFC 7638 thumbprint helpers, implemented with the
standard library because this is the device's own code. It is the production equivalent of the
development client in `localdev/authlab/jose.go`, lifted out so capture-core can use it without
importing a development package.
