Changelog
=========

# 1.0.1

* Bug fix for #181: Shuffle peers on PeerList.Add to avoid biases in peer
  selection.
* Peers can now be removed using PeerList.Remove.
* Add ErrorHandlerFunc to create raw handlers that return errors.
* Retries try to avoid previously selected hosts, rather than just the
  host:port.
* Created an ArgReader interface (which is an alias for io.ReadCloser) for
  symmetry with ArgWriter.
* Add ArgReadable and ArgWritable interfaces for the common methods between
  calls and responses.
* Expose Thrift binary encoding methods (thrift.ReadStruct, thrift.WriteStruct,
  thrift.ReadHeaders, thrift.WriteHeaders) so callers can easily send Thrift
  payloads over the streaming interface.

# 1.0.0

* First stable release.
* Supports making calls with JSON, Thrift or raw payloads.
* Services use thrift-gen, and implement handlers wiht a `func(ctx, arg) (res,
  error)` signature.
* Supports retries.
* Peer selection (peer heap, prefer incoming strategy, for use with Hyperbahn).
* Graceful channel shutdown.
* TCollector trace reporter with sampling support.
* Metrics collection with StatsD.
* Thrift support, including includes.