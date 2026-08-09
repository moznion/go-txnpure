# Changelog

## [v0.2.0](https://github.com/moznion/go-txnpure/compare/v0.1.0...v0.2.0) - 2026-08-09

- Export the transaction state machine as Session for native-driver integrations by @moznion in https://github.com/moznion/go-txnpure/pull/9

## [v0.1.0](https://github.com/moznion/go-txnpure/compare/v0.0.1...v0.1.0) - 2026-08-05

- Add at-the-effect-site allows (`AllowInTransactionHere`) and exact in-transaction call counts on every allow form by @moznion in https://github.com/moznion/go-txnpure/pull/6
- releng: bump new version by @moznion in https://github.com/moznion/go-txnpure/pull/8

## [v0.0.1](https://github.com/moznion/go-txnpure/commits/v0.0.1) - 2026-07-26

- Activate Songmu/tagpr by @moznion in https://github.com/moznion/go-txnpure/pull/2
- Make the always-on hot paths allocation-free and gate CI against perf regressions by @moznion in https://github.com/moznion/go-txnpure/pull/1
- Drop the benchmark job from CI, keep the deterministic gate by @moznion in https://github.com/moznion/go-txnpure/pull/4
- Add a fuzz suite for crash safety by @moznion in https://github.com/moznion/go-txnpure/pull/5
