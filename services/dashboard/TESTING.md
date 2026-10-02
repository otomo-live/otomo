# Dashboard test map

Every acceptance criterion for this service, and the tests that hold it. This file maps
`design/01-dashboard.md` §5 (DSH-C1…C12) and the plan's DSH-H1 (the health prober /
`GET /services`) onto the suite. DSH-E1 is the suite
itself.

Run everything:

```sh
gofmt -l .
go vet ./...
go test -race -count=2 ./...
go test -cover -coverpkg=./... ./...
```

The PromQL substitution golden is regenerated with:

```sh
go test ./internal/promql -update
```

| ID | Criterion | Tests |
|---|---|---|
| DSH-C1 | Service from the template; Gateway route `/api/admin/dashboard/*` registered for the staff issuer | `api.TestRouteTableIsWellFormed`, `api.TestHealthz`, `api.TestReadyz`; `server.TestPublicListenerAdmitsOnlyStaffTokens`, `server.TestInternalEndpointsStayOnTheInternalListener`; `config.Test*` |
| DSH-C2 | Re-verify the staff token and role in-process | `server.TestPublicListenerAdmitsOnlyStaffTokens`, `server.TestStaffWithoutRolesIsForbidden`, `server.TestRejectionsAreCountedByReason`, `server.TestUnmatchedRequestsAreGuardedToo`; `auth.TestVerify*`, `auth.TestHasRoleAtLeast`, `auth.TestClaimsRoundTripThroughContext` |
| DSH-C3 | Static PromQL template catalogue; service allow-list + grammar before substitution | `promql.TestEveryTemplateBuilds`, `promql.TestBuildWindowFloor`, `promql.TestBuildRejectsService`, `promql.TestServiceSetContains`, `promql.TestTemplateSubstitutionGolden` (+ `testdata/templates.golden`), `promql.TestOverviewGolden`, `promql.TestOverviewHasNoPlaceholders`; `api.TestSeriesUnknownMetric`, `api.TestSeriesUnknownService` |
| DSH-C4 | Prometheus client converts `/api/v1/query` and `/query_range` to the columnar shape | `prometheus.TestRangeColumnar` (recorded `testdata/range.json`), `prometheus.TestInstantVectorAndScalar` (`vector.json`, `scalar.json`), `prometheus.TestInstantEmptyResult`, `prometheus.TestTargets` (`targets.json`), `prometheus.TestSeriesKey`, `prometheus.TestLabelValue`, `prometheus.TestParseValueSpecial`, `prometheus.TestRangeRequestParams`, `prometheus.TestErrorMapping`, `prometheus.TestAllowList*` |
| DSH-C5 | `GET /overview`: one concurrent batch, merged, degrading per card | `api.TestOverviewAllFieldsMapped`, `api.TestOverviewOneQueryFails`, `api.TestOverviewSlowQueryDegrades`, `api.TestOverviewNilMetricsIsAllDegraded`, `api.TestOverviewDegradedIsSortedAndDeduped`, `api.TestOverviewHandlerOverhead`, `api.TestOverviewServicesUnionAndSort`; `server.TestOverviewEndpointThroughTheServer` |
| DSH-C6 | `GET /services/{name}/series`: 7-day clamp and ≤1500 points | `api.TestSeriesStepCalculation`, `api.TestSeriesAlignmentAndPointCap`, `api.TestSeriesClamp30Days`, `api.TestSeriesColumnarShape`, `api.TestSeriesBuildsRateWindow`, `api.TestSeriesRFC3339AndFutureClamp`, `api.TestSeriesValidation`; `server.TestSeriesEndpointThroughTheServer` |
| DSH-C7 | Response cache (endpoint, params, time bucket), 10 s TTL, bounded, single-flight | `cache.TestDoCachesWithinTTL`, `cache.TestDoExpiresAfterTTL`, `cache.TestDoSingleflight`, `cache.TestDoDistinctKeysLoadSeparately`, `cache.TestBoundEvictsOldest`, `cache.TestCallerCancellationDoesNotAbortBatch`; `api.TestOverviewConcurrentRequestsRunOneBatch`, `api.TestOverviewSecondRequestHitsCacheThenBucketRolls`, `api.TestSeriesCacheHit` |
| DSH-C8 | LogQL from validated labels; `contains` escaped as a literal line filter; limit ≤1000 | `loki.TestBuildGolden`, `loki.TestBuildValidation`; `loki.TestQueryMergesStreamsNewestFirst`, `loki.TestQueryJSONLineFieldsAndMessage`, `loki.TestQueryLimitCutsNewest`, `loki.TestQueryErrorBody` (recorded `testdata/query_range.json`); `api.TestLogsValidation`, `api.TestLogsDefaults`, `api.TestLogsHappyPathShape`, `api.TestLogsUnknownServiceDoesNotQuery`, `api.TestLogsUpstreamError` |
| DSH-C9 | SSE `GET /logs/tail`: caps 2/user and 10 global, clean close on disconnect | `api.TestTailLimiterPerUser`, `api.TestTailLimiterGlobal`, `api.TestTailLimiterReleaseIsIdempotent`, `api.TestTailLimiterDefaults`; `server.TestTailStreamsThroughTheServer`, `server.TestTailPerUserCap`, `server.TestTailGlobalCap` |
| DSH-C10 | SSE keep-alive and no response buffering | `server.TestTailKeepAlive`; headers asserted in `server.TestTailStreamsThroughTheServer`. The proxy's read timeout / buffering is Gateway configuration, outside this service |
| DSH-C11 | `GET /audit`: fan out to both feeds with the caller's token, merge by time, combined cursor | `auditsrc.TestFetchForwardsOnlyTheToken`, `auditsrc.TestFetchQueryParameters`, `auditsrc.TestFetchPropagatesAuthStatus`, `auditsrc.TestFetchStrictDecode`, `auditsrc.TestFetchCapsBody`, `auditsrc.TestFetchNullCursorIsEmpty`; `api.TestAuditMergeInterleavesOnePage`, `api.TestAuditWalkIsExact`, `api.TestAuditResumeSurvivesNewRows`, `api.TestAuditForwardsTokenAndNothingElse`, `api.TestAuditAuthStatusPropagates`, `api.TestAuditDegradedOnOneFailure`, `api.TestAuditAllFailIs502`, `api.TestAuditSourceSelection`, `api.TestAuditValidation`; `server.TestAuditEndpointThroughTheServer` |
| DSH-C12 | Dashboard's own metrics: upstream latency, cache hit ratio, degraded cards, tail gauge | `server.TestMetricsPreInitialised`, `server.TestInstrumentMethodsRecord`, `server.TestOverviewDegradedMetric`, `server.TestTailStreamsGauge`; `prometheus.TestObserverRecordsUpstreamResult`, `loki.TestObserverRecordsUpstreamResult`, `auditsrc.TestObserverUpstreamNames`, `cache.TestDoObserverCountsHitMissShared`, `api.TestOverviewDegradedObserver`, `api.TestOverviewCacheMetricsHitMissShared` |
| DSH-H1 | Health prober polls every target's `/readyz`; `GET /services` reports the snapshot and the probe metrics | `health.TestSnapshotBeforeFirstProbe`, `health.TestSnapshotIsSortedByName`, `health.TestProbeReady`, `health.TestProbeNotReadyCOM5Body`, `health.TestProbeNotReadyPlainBody`, `health.TestProbeConnectionRefused`, `health.TestProbeTimeout`, `health.TestProbeDoesNotFollowRedirects`, `health.TestProbeTruncatesLargeBody`, `health.TestProbeRecoversAndSetsLastReadyAt`, `health.TestProbesRunConcurrently`, `health.TestSnapshotIsSortedByName`, `health.TestStatusJSONCheckedAtNullBeforeFirstProbe`, `health.TestFailureReasonTruncatesOnRuneBoundary`; `server.TestServicesEndpointThroughTheServer`, `server.TestServiceProbeMetrics` |
| DSH-E1 | Unit tests over recorded fixtures for substitution, step, conversion and merge | This suite: `promql/testdata/templates.golden`, `prometheus/testdata/{range,vector,scalar,targets,error}.json`, `loki/testdata/{query_range,label_values,error}.json`, and the audit merge property test `api.TestAuditWalkIsExact` |
