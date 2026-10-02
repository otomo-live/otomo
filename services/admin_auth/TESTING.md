# admin-auth testing matrix

Maps every admin-plane acceptance criterion (AA-1..AA-10) and decisions D2, D3 and D5
to the tests that cover it. Test names are `file:TestName`; the file is relative to the
package directory unless prefixed.

Run everything (the DB-backed packages skip when the variable is unset):

```sh
gofmt -l . && go vet ./...
ADMIN_AUTH_TEST_DATABASE_URL='postgres://auth_rw:pw@127.0.0.1:5433/admin_auth_test?sslmode=disable' \
  go test -race -count=2 ./...
```

`internal/store`, `internal/bootstrap` and the DB-backed `internal/server` tests share
one throwaway database and serialise themselves with a Postgres advisory lock. Timing
assertions (`*Indistinguishable`, `TestDummyVerifiesInComparableTime`) are skipped under
`-short`.

## AA-1 — service skeleton: config, listeners, COM-5 errors, request IDs, metrics, readiness

| Test |
|---|
| `internal/config/config_test.go:TestLoadEmptyEnvNamesEveryRequiredVar` |
| `internal/config/config_test.go:TestLoadReportsEveryProblemAtOnce` |
| `internal/config/config_test.go:TestLoadRejectsNonPositiveDuration` |
| `internal/config/config_test.go:TestLoadServeDefaults` |
| `internal/config/config_test.go:TestLoadServeRequiresTOTPKey` |
| `internal/config/config_test.go:TestLoadServeRejectsBadSizeTOTPKey` |
| `internal/config/config_test.go:TestLoadServeRejectsUnreadableTOTPKey` |
| `internal/config/config_test.go:TestLoadGenkeyDoesNotRequireSigningKeyPath` |
| `internal/config/config_test.go:TestLoadMigrateRequiresDatabaseURL` |
| `internal/config/config_test.go:TestLoadBootstrapRootNeedsOnlyDatabaseURL` |
| `internal/api/api_test.go:TestWriteErrorShapeCarriesRequestID` |
| `internal/api/api_test.go:TestHealthzAlwaysOK` |
| `internal/api/api_test.go:TestReadyz` |
| `internal/api/api_test.go:TestFallback404And405InCOM5Shape` |
| `internal/server/server_test.go:TestRequestIDIsGeneratedWhenAbsentAndEchoedWhenPresent` |
| `internal/server/server_test.go:TestUnknownPathIsCOM5NotFound` |
| `internal/server/server_test.go:TestInternalEndpointsAreNotOnThePublicListener` |
| `internal/server/server_test.go:TestMetricsExposeBuildInfoAndRequests` |
| `internal/server/server_test.go:TestReadyzTracksTheReadyFlag` |
| `internal/server/server_test.go:TestReadyzFailsWhenNoSigningKeysAreLoaded` |
| `internal/server/server_test.go:TestReadyzFailsWhenTheDatabaseIsUnreachable` |

## AA-2 — staff schema and constraints (migration `00002`)

| Test |
|---|
| `internal/store/store_test.go:TestMigrateIsIdempotent` |
| `internal/store/store_test.go:TestMigrateUpDownUp` |
| `internal/store/store_test.go:TestEmailUniquenessIsCaseInsensitive` |
| `internal/store/store_test.go:TestRolesCheck` |
| `internal/store/store_test.go:TestSingleRoot` |
| `internal/store/store_test.go:TestInviteChecks` |
| `internal/store/store_test.go:TestStaffDeleteCascades` |

## AA-3 — signing key, JWKS publication, access-token issuance

| Test |
|---|
| `internal/token/token_test.go:TestKeyFileRoundTrip` |
| `internal/token/token_test.go:TestLoadRejectsUnusableFiles` |
| `internal/token/token_test.go:TestBuildJWKSMatchesTheContractShape` |
| `internal/token/token_test.go:TestIssueVerifiesWithTheGatewaysParserOptions` |
| `internal/token/token_test.go:TestIssueRejectsEmptyRoles` |
| `internal/token/contract_test.go:TestIssuedTokenMatchesTheConsumerContract` |
| `internal/token/verifier_test.go:TestVerifierAcceptsItsOwnToken` |
| `internal/token/verifier_test.go:TestVerifierRejections` |
| `internal/token/verifier_test.go:TestReasonForMapsUnknownErrorsToInvalidToken` |
| `internal/api/api_test.go:TestJWKSHandlerServesCachedBytes` |
| `internal/server/server_test.go:TestJWKSRouteServesThePublishedKey` |
| `internal/store/store_test.go:TestSigningKeyRoundTrip` |
| `internal/store/store_test.go:TestInsertSigningKeyRejectsADuplicateKid` |
| `internal/store/store_test.go:TestFindActiveByPublicKeyReportsNotFound` |
| `admin_auth_test.go:TestGenkeyTOTPWritesAKeyAndRefusesToOverwrite` |

## AA-4 — `POST /admin-auth/login`

| Rule | Test |
|---|---|
| 200 shape, cookie attributes, sha256 stored, audit | `internal/server/login_test.go:TestLoginSuccess` |
| case-insensitive email | `internal/server/login_test.go:TestLoginEmailIsCaseInsensitive` |
| one 401 for every credential failure | `internal/server/login_test.go:TestLoginFailuresShareOneResponse` |
| unknown email vs wrong password: same body modulo `request_id`, comparable argon2 cost | `internal/server/login_test.go:TestLoginUnknownEmailAndWrongPasswordAreIndistinguishable` |
| 5 failures lock; right password refused while locked; expiry recovers | `internal/server/login_test.go:TestLoginLockoutAndRecovery` |
| success clears the failure counter | `internal/server/login_test.go:TestLoginSuccessResetsFailureCount` |
| exactly one argon2id per attempt (incl. unknown email dummy) | `internal/server/login_test.go:TestLoginVerifyRunsExactlyOncePerAttempt` |
| malformed bodies (16 KiB cap) | `internal/server/login_test.go:TestLoginBadBodies` |
| hostile User-Agent cannot break a login | `internal/server/login_test.go:TestLoginSurvivesHostileUserAgent` |
| argon2id params, salt uniqueness, wrong password, malformed PHC | `internal/password/password_test.go` (`TestHashVerifyRoundTrip`, `TestHashWritesConfiguredParamsAndAFreshSalt`, `TestVerifyRejectsWrongPassword`, `TestVerifyRejectsMalformedHash`, `TestVerifyParsesParamsFromTheHash`, `TestVerifyRejectsExcessiveParameters`) |
| `Dummy()` stable and comparable in cost | `internal/password/password_test.go` (`TestDummyIsStableAndValid`, `TestDummyVerifiesInComparableTime`) |
| login metric labels | `internal/server/metrics_test.go:TestLoginMetricsAreCountedByOutcome` |

## AA-5 — refresh, logout, `/me`

| Rule | Test |
|---|---|
| rotation: new cookie, old row rotated, successor in family | `internal/server/session_test.go:TestRefreshRotates` |
| N=10 concurrent refreshes: exactly one rotation (one new row / one Set-Cookie), rest grace path | `internal/server/session_test.go:TestRefreshConcurrentExactlyOneRotation` |
| grace disabled: exactly one 200, others 401 | `internal/server/session_test.go:TestRefreshGraceDisabledAllowsExactlyOneRotation` |
| benign reuse within grace: 200, no cookie, nothing revoked | `internal/server/session_test.go:TestRefreshReuseWithinGrace` |
| reuse after grace revokes the whole family | `internal/server/session_test.go:TestRefreshReuseAfterGraceRevokesTheFamily` |
| dead/expired/revoked cookie | `internal/server/session_test.go:TestRefreshRejectsDeadCookies` |
| demotion takes effect at the next refresh | `internal/server/session_test.go:TestRefreshRereadsRolesFromTheDatabase` |
| disabled user's refresh fails and revokes the family | `internal/server/session_test.go:TestRefreshDisabledUserRevokesFamily` |
| grace cannot resurrect a disabled user | `internal/server/session_test.go:TestRefreshGraceDoesNotResurrectADisabledUser` |
| logout revokes the family and always clears | `internal/server/session_test.go:TestLogoutRevokesFamilyAndAlwaysClears` |
| `/me` reads the DB, not the token | `internal/server/session_test.go:TestMeReadsTheUserFromTheDatabase` |
| `/me` rejection codes | `internal/server/session_test.go:TestMeRejections` |
| refresh metric labels | `internal/server/metrics_test.go:TestRefreshMetricsAreCountedByOutcome` |

## AA-6 — `bootstrap-root`

| Test |
|---|
| `internal/bootstrap/bootstrap_test.go:TestRunCreatesRoot` |
| `internal/bootstrap/bootstrap_test.go:TestRunIsIdempotent` |
| `internal/bootstrap/bootstrap_test.go:TestRunRotatesRoot` |
| `internal/bootstrap/bootstrap_test.go:TestRunRotateWithNoRootCreates` |
| `internal/bootstrap/bootstrap_test.go:TestReadPasswordFileRules` |
| `internal/bootstrap/bootstrap_test.go:TestRunConcurrentBootstrap` |
| `internal/bootstrap/bootstrap_test.go:TestBootstrapRootCanLogIn` |

## AA-7 — staff user-management API

| Rule | Test |
|---|---|
| admin gate: no token 401; viewer/live_ops 403; disabled admin 401 | `internal/server/admin_users_test.go:TestAdminUsersAuth` |
| list never leaks a hash or TOTP secret | `internal/server/admin_users_test.go:TestAdminListUsersExcludesSecrets` |
| invite shape, sha256 stored, fragment URL, audit | `internal/server/admin_users_test.go:TestAdminInviteCreatesAndHashesTheToken` |
| invite validation, conflicts, non-root admin 403 | `internal/server/admin_users_test.go:TestAdminInviteValidationAndConflicts` |
| revoke 204 then 404 | `internal/server/admin_users_test.go:TestAdminInviteRevoke` |
| PATCH matrix (demote, promote, disable admin, root/self protection) | `internal/server/admin_users_test.go:TestAdminPatchMatrix` |
| reset link, replaces pending, root/admin rules | `internal/server/admin_users_test.go:TestAdminResetLink` |
| admin reset of another user's MFA: rule matrix, full effect, admin re-enroll, concurrent reset | `internal/server/admin_mfa_test.go:TestAdminResetMFARules`, `TestAdminResetMFAEffect`, `TestAdminResetMFAAdminMustReenroll`, `TestAdminResetMFAConcurrent` |

## AA-8 — onboarding (lookup and redeem)

| Test |
|---|
| `internal/server/onboard_test.go:TestOnboardLookup` |
| `internal/server/onboard_test.go:TestOnboardInviteRedeem` |
| `internal/server/onboard_test.go:TestOnboardResetRedeem` |
| `internal/server/onboard_test.go:TestOnboardPasswordRules` |
| `internal/server/onboard_test.go:TestOnboardConcurrentRedeem` |
| `internal/server/onboard_test.go:TestOnboardInviteEmailConflict` |
| `internal/server/onboard_test.go:TestOnboardBadBodies` |
| onboard applies the login MFA policy: admin invite demands enrollment, reset with a confirmed factor demands verify | `internal/server/onboard_test.go:TestOnboardInviteAdminRequiresEnrollment`, `TestOnboardResetWithConfirmedTOTPRequiresVerify` |

## AA-9 — TOTP MFA

| Rule | Test |
|---|---|
| RFC 6238 vectors, window, secrets, recovery codes, sealing | `internal/mfa/mfa_test.go` (`TestRFC6238SHA1Vectors`, `TestValidateAcceptsOneStepEitherSide`, `TestSealOpenRoundTrip`, `TestOpenRejectsWrongKeyAndTampering`, `TestOpenRejectsWrongAADUser`, `TestSecretEncodingRoundTrips`, `TestOTPAuthURLFormat`, `TestRecoveryCodesAreWellFormedAndUnique`, `TestNormalizeRecoveryCodeRejectsNonCodes`, `TestHashRecoveryCodeIsSHA256OfTheNormalizedCode`, `TestKeyFileRoundTripAndPermissions`, `TestWriteKeyRefusesToOverwrite`, `TestLoadKeyRejectsBadFiles`) |
| root exempt, confirmed factor challenges, admin must enroll, viewer not | `internal/server/mfa_test.go:TestLoginRootWithConfirmedFactorStillGetsTokens`, `TestLoginConfirmedFactorChallengesWithNoCookie`, `TestLoginAdminWithoutTOTPRequiresEnrollment`, `TestLoginViewerWithoutTOTPStillGetsTokens` |
| verify TOTP / recovery, replay, attempt limit, ticket expiry/purpose | `internal/server/mfa_test.go:TestMFAVerifyTOTPIssuesSession`, `TestMFAVerifyRejectsReplayedTOTP`, `TestMFAVerifyRecoveryCodeWorksOnce`, `TestMFAVerifyBurnsTicketAfterFiveWrongCodes`, `TestMFAVerifyRejectsExpiredTicket`, `TestMFAVerifyRejectsEnrollTicket`, `TestMFAVerifyRejectsAccountDisabledMeanwhile` |
| enroll/confirm by ticket and bearer, root 403, already enabled 409 | `internal/server/mfa_test.go:TestMFAEnrollConfirmTicketFlow`, `TestMFAEnrollConfirmViaBearer`, `TestMFARootCannotEnroll`, `TestMFAEnrollRejectsConfirmedFactor`, `TestMFAConfirmRejectsNoPendingSecret`, `TestMFASecretsAreSealedPerUser` |
| concurrent guesses bounded; metrics | `internal/server/mfa_test.go:TestMFAVerifyAttemptsAreBoundedUnderConcurrency`, `TestMFAMetricsSeriesArePreInitialised`, `TestMFAMetricsAreCountedByOutcome` |

## AA-10 — audit feed and login/refresh metrics

| Test |
|---|
| `internal/server/audit_test.go:TestAuditFeedCarriesTheSharedShape` |
| `internal/server/audit_test.go:TestAuditPagingWalksEveryRowWithoutDuplicates` |
| `internal/server/audit_test.go:TestAuditFilters` |
| `internal/server/audit_test.go:TestAuditRejectsMalformedPaging` |
| `internal/server/audit_test.go:TestAuditRejections` |
| `internal/server/metrics_test.go:TestLoginMetricsAreCountedByOutcome` |
| `internal/server/metrics_test.go:TestRefreshMetricsAreCountedByOutcome` |

## Decision D2 — role model and DB-sourced authorisation boundary

`admin` gates the management API; `viewer` is the minimum for audit; every request
re-reads the caller's row so a demotion or disable takes effect immediately; the root
account is protected.

| Test |
|---|
| `internal/server/admin_users_test.go:TestAdminUsersAuth` |
| `internal/server/admin_users_test.go:TestAdminRoleGrantMatrix` |
| `internal/server/admin_users_test.go:TestAdminPatchMatrix` |
| `internal/server/admin_users_test.go:TestAdminResetLink` |
| `internal/server/session_test.go:TestMeRejections` |
| `internal/server/audit_test.go:TestAuditRejections` |
| `internal/server/mfa_test.go:TestMFAEnrollConfirmViaBearer` |

## Decision D3 — role-grant matrix (root / admin / live_ops / viewer)

Actor × action through the HTTP API: invite viewer, invite live_ops, invite admin,
PATCH roles→admin, PATCH own roles, disable root, disable self, disable other admin.
`admin` granting `admin` is 403; root may grant it; nobody changes their own roles;
root cannot be disabled; non-admins are 403 on all of it.

| Test |
|---|
| `internal/server/admin_users_test.go:TestAdminRoleGrantMatrix` (the single table-driven matrix) |
| `internal/server/admin_users_test.go:TestAdminPatchMatrix` |
| `internal/server/admin_users_test.go:TestAdminInviteValidationAndConflicts` |
| `internal/server/admin_users_test.go:TestAdminResetLink` |

## Decision D5 — MFA policy (TOTP required for `admin`, optional otherwise, root exempt)

| Test |
|---|
| `internal/server/mfa_test.go:TestLoginRootWithConfirmedFactorStillGetsTokens` |
| `internal/server/mfa_test.go:TestLoginConfirmedFactorChallengesWithNoCookie` |
| `internal/server/mfa_test.go:TestLoginAdminWithoutTOTPRequiresEnrollment` |
| `internal/server/mfa_test.go:TestLoginViewerWithoutTOTPStillGetsTokens` |
| `internal/server/mfa_test.go:TestMFARootCannotEnroll` |
| `internal/server/mfa_test.go:TestMFAEnrollRejectsConfirmedFactor` |
| `internal/server/mfa_test.go:TestMFAVerifyRejectsAccountDisabledMeanwhile` |

## Self-service account endpoints

Every `/admin-auth/account/*` route is bearer-authenticated like `/me` — the user is
re-read from the database and a disabled account is refused — and uses the strict JSON
body decoder.

| Rule | Test |
|---|---|
| password: wrong current 401 and lockout after N, weak new 400, same-as-current 400, success 204 revokes other sessions and keeps the caller's, old password fails login while the new works, audit | `internal/server/account_test.go:TestAccountPasswordChange`, `TestAccountPasswordChangeLockout` |
| sessions: two logins listed newest-first with `current` on the cookie's row; revoke-others keeps the current and revokes the other; without a cookie revokes all | `internal/server/account_test.go:TestAccountSessionsListAndRevoke` |
| recovery codes: fresh TOTP required (replay refused), old codes stop working, ten new ones work once each, audit | `internal/server/account_test.go:TestAccountRecoveryCodesRegenerate` |
| disable: TOTP or recovery code 204 then login has no MFA challenge; admin 403 `mfa_required`; root 403 `root_protected`; not enrolled 409; wrong code 401; audit | `internal/server/account_test.go:TestAccountDisableMFA` |
| all endpoints: no/invalid token 401 and disabled account 401 | `internal/server/account_test.go:TestAccountEndpointsRequireActiveBearer` |

## Gaps found and filled later

| Gap | New test |
|---|---|
| D3 complete 4-actor × 8-action matrix | `internal/server/admin_users_test.go:TestAdminRoleGrantMatrix` |
| salt uniqueness | `internal/password/password_test.go:TestHashWritesConfiguredParamsAndAFreshSalt` |
| `Dummy()` cost comparable to a real verify | `internal/password/password_test.go:TestDummyVerifiesInComparableTime` |
| login bodies identical modulo `request_id` + timing bound | `internal/server/login_test.go:TestLoginUnknownEmailAndWrongPasswordAreIndistinguishable` |
| success resets the lockout counter | `internal/server/login_test.go:TestLoginSuccessResetsFailureCount` |
| N=10 concurrent refresh, precise row/cookie shape | `internal/server/session_test.go:TestRefreshConcurrentExactlyOneRotation` |
| grace disabled: one 200, rest 401 | `internal/server/session_test.go:TestRefreshGraceDisabledAllowsExactlyOneRotation` |

No open gaps remain across AA-1..AA-10 and D2/D3/D5. MFA was already fully covered by
the MFA tests; onboarding only extended the same login policy to onboarding
(AA-8).
