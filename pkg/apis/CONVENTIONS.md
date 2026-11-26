# `pkg/apis` Conventions: Merging, Normalization, Defaulting, and Validation

This document defines the contract for `NetworkConfig` across `pkg/apis` and `pkg/driver`.
Every rule below is enforced by unit and pipeline tests so new fields cannot silently drift.

## 1. Configuration Pipeline and Precedence

DRANET assembles the effective `NetworkConfig` for a device in four stages during `NodePrepareResources`:

```text
  User Claim (JSON)           Cloud DeviceConfig (cached)
         │                               │
         ▼                               ▼
  Stage 1: Intake (non-mutating)   Stage 1: Intake (non-mutating)
    - ValidateConfigIntake(raw):     - ValidateIntake():
      strict JSON unmarshal +          Normalize() (deprecated) +
      Normalize() + ValidateIntake()   single-source syntax/ranges
         │                               │
         └──────────────┬────────────────┘
                        ▼
  Stage 2: Merge static sources (pure deep-copy, bottom-up)
    merged = MergeNetworkConfigs(user, cloud)
                        │
                        ▼  (if merged.Profile != "")
  Stage 3: Profile resolution
    - GetProfileConfig(device, claim, merged.Clone())
        receives cloned, normalized, non-defaulted intent; may deny with an error
    - Stage 1 Intake on profileConf
    - merged = MergeNetworkConfig(merged, profileConf)   [lowest precedence]
        on intake error: ReleaseProfileConfig + join errors
                        │
                        ▼
  Stage 4: Finalize (runs ONCE after all merges)
    - snapshot releaseConf = merged.Clone() (pre-defaulted, for rollback)
    - merged.Validate():
        1. Normalize() safety net
        2. Default() fallback values (IPVlan, VRF table; skipped if normalize failed)
        3. Full completeness and cross-field validation
    - on validation error after profile allocation: ReleaseProfileConfig(releaseConf) + join errors
```

### Source Roles and Precedence

Precedence from highest to lowest:

1. **User configuration** (from the `ResourceClaim` opaque parameters) — workload intent.
2. **Cloud device configuration** (from `GetDeviceConfig`) — infrastructure baseline for the hardware.
3. **Profile configuration** (from `GetProfileConfig`) — dynamic enrichment (IPAM, policy-based routing, host-derived defaults). A profile provider fills unset fields and combines slices (lower-precedence entries first; higher precedence wins on canonical duplicates); it cannot silently override higher-precedence scalar or pointer settings. To forbid an unsupported user setting, the profile provider returns an error.

> **Note on multiple opaque configs (bottom-up static layering):** Because dependent-field and unit rules (`VRF` replacement, `IPVlan` drop on `Interface.Type` change) are non-associative across 3+ layers, static sources must be folded **bottom-up** (right to left) using `MergeNetworkConfigs(claim, deviceClass, cloud)` rather than left-folding `MergeNetworkConfig(MergeNetworkConfig(claim, deviceClass), cloud)`. Bottom-up folding ensures intermediate layers combine with lower layers before a higher layer overrides a VRF domain or interface type.

### Failure and Rollback

- An invalid cloud `DeviceConfig` fails at Stage 1 **before** `GetProfileConfig` is called, avoiding leaked profile allocations.
- `GetProfileConfig` receives `merged.Clone()`, so an in-process provider cannot mutate `merged` and bypass higher-precedence settings.
- After `GetProfileConfig` succeeds, any Stage 3 profile intake error or Stage 4 final validation error calls `ReleaseProfileConfig` with the pre-defaulted merged allocation details (so IPAM leases can be freed with a consistent shape across both stages) and joins the release error if cleanup also fails.

---

## 2. Normalization vs. Fallback Defaulting

DRANET separates **normalization** (rewriting deprecated aliases) from **fallback defaulting** (deriving omitted values):

| Operation | Function | When It Runs | Mutates Input? | Purpose |
| :--- | :--- | :--- | :--- | :--- |
| **Normalization** | `(*NetworkConfig).Normalize()` | Stage 1 (Intake), per source | **No** (returns deep copy) | Fold deprecated fields into canonical fields and clear the deprecated field so merging and providers see one schema. On conflict, returns the cloned config with the conflicting deprecated field untouched alongside errors. |
| **Intake Validation** | `(*NetworkConfig).ValidateIntake()` / `ValidateConfigIntake(raw)` | Stage 1 (Intake), per source | **No** (returns deep copy) | Strict JSON unmarshal (raw input), syntax, ranges, enum values, and single-source conflicts. Allows partial cross-source blocks (e.g., `vrf: {table: 200}`, `ipvlan: {mode: L2}`, `addressing: Unnumbered` without `type`). On error, returns the normalized deep copy (or `nil` on JSON syntax error) alongside errors so rollback callers can inspect allocated fields. |
| **Fallback Defaulting** | `(*NetworkConfig).Default()` | Stage 4 (Finalize), **once** after all merges | Yes (on owned merged copy) | Derive omitted dependent values: `IPVlan` (`L2`, `Bridge`) when `Type == IPVLAN`, and `VRF.Table = TableIDForName(Name)` when `Table == nil && Name != ""`. |
| **Final / Standalone Validation** | `(*NetworkConfig).Validate()` / `ValidateConfig(raw)` | Stage 4 (Finalize) or standalone single-config callers | Yes (normalizes, defaults if normalize succeeds, validates) | Enforce completeness (`VRF.Name != ""`) and all cross-field invariants (`VRF` vs `Rules`, `IPVlan` vs `Type`, `Unnumbered` vs subinterface, `MTU` vs IPv6 sysctls, `DHCP` vs `Addresses`). `ValidateConfig(raw)` preserves standalone full validation + defaulting; multi-source pipelines use `ValidateConfigIntake(raw)`. |

Running fallback defaults once at Stage 4 guarantees that a derived default never overrides an explicit value from a lower-precedence source (for example, a user `vrf: {name: "a"}` preserves a provider's explicit `vrf: {name: "a", table: 100}`).

Because `netdb.GetDeviceConfig` returns a shared cached `*NetworkConfig`, `Normalize()` and `ValidateIntake()` must never mutate their receiver.

### Deprecated `interface.dhcp` Normalization Rules

Intake applies these rules to every source and clears `interface.dhcp` to `nil` when valid:

| Input `dhcp` | Input `addressing` | Normalized Result |
| :--- | :--- | :--- |
| `nil` | any | unchanged |
| `true` | `""` or `"DHCP"` | `addressing: "DHCP"`, `dhcp: nil` |
| `true` | any other value | **intake error** (conflict; `dhcp` untouched) |
| `false` | `""` | `addressing: "Static"`, `dhcp: nil` |
| `false` | `"DHCP"` | **intake error** (conflict; `dhcp` untouched) |
| `false` | `"Static"` or `"Unnumbered"` | `addressing` unchanged, `dhcp: nil` |
| `false` | unrecognized enum string | `dhcp` untouched (intake validation rejects the unknown addressing mode) |

Mapping `dhcp: false` (with unset `addressing`) to `"Static"` ensures an explicit `dhcp: false` overrides a provider's `dhcp: true` or `addressing: "DHCP"`. As a trade-off, `dhcp: false` also overrides a provider's `addressing: "Unnumbered"`; callers that want non-DHCP subinterface addressing should use `addressing` directly.

---

## 3. Merge Rules (`MergeNetworkConfig` / `MergeNetworkConfigs`)

`MergeNetworkConfig(user, cloud)` merges every field explicitly by name (no reflection merge libraries such as `mergo`). Callers must normalize inputs first (via `Normalize()` or `ValidateIntake()`) so deprecated fields such as `Interface.DHCP` are already folded into canonical fields before merging. When combining three or more static layers, use `MergeNetworkConfigs(layers...)` (ordered highest to lowest precedence) so layers fold bottom-up. A `nil` input is treated as an empty `NetworkConfig`. Neither input is mutated, and the result shares no mutable memory with either input.

| Field Kind | Rule | Examples |
| :--- | :--- | :--- |
| **String / string enum** | First non-empty (`user` wins over `cloud`). | `Profile`, `Interface.Name`, `Type`, `Addressing` |
| **Scalar pointer** | Fresh copy of `user` value when non-nil—**including explicit zero values (`false`, `0`, `""`)**; else fresh copy of `cloud` value. | `MTU`, `HardwareAddr`, `ARPIgnore`, `ARPAnnounce`, `ARPNotify`, `AddrGenMode`, `AcceptRA`, `Forwarding`, `DisableEBPFPrograms`, GSO/GRO sizes |
| **Slice** | Concatenate `cloud` first and `user` last, then deduplicate by canonical kernel identity keeping the last occurrence so `user` wins (unparseable values fall back to raw equality so validation still reports them). | `Addresses` (canonical `netip.Prefix`, IPv6 keyed at `/128`), `Routes` (masked CIDR + effective table with `0 -> 254`, or destination-only when merged `Interface.VRF != nil`), `Rules` (`priority`, masked `source`, masked `destination`, effective table `0 -> 254`), `Neighbors` (canonical unmapped `netip.Addr`) |
| **Map** | Allocate a new map containing all `cloud` entries overlaid by `user` entries (`user` key wins). | `Ethtool.Features`, `Ethtool.PrivateFlags` |
| **Independent nested struct pointer** | Allocate a new struct if either side is non-nil (including empty `{}`), and merge field by field. | `Ethtool`, `IPVlan` (when active) |
| **Unit struct pointer (identity-bearing)** | Keyed by an identity field whose dependent fields belong to that identity. When both sides set different non-empty identities, the `user` block replaces the `cloud` block wholesale (leaving unset dependent fields `nil` for Stage 4 defaulting). When identities match or only one side sets it, merge field by field. | `VRF` (identity: `Name`; dependent: `Table`) |
| **Presence-only struct pointer** | Allocate a new empty struct if either side is non-nil. | Empty marker blocks (`struct{}`) |
| **Type-dependent nested struct pointer** | Drop `cloud`'s dependent block **only** when `cloud` set the matching discriminator type and `user` explicitly switches to a different type. Never silently drop an explicit `user` block or a misconfigured `cloud` block without the matching type; Stage 4 validation rejects it. Switching `IPVLAN -> Passthrough` intentionally keeps `Addressing: Unnumbered` so Stage 4 requires the user to choose an explicit addressing mode. | `IPVlan` (discriminator: `Interface.Type == IPVLAN`) |

### Unit Type Definition (`VRF`)

A struct is a **unit** when its other fields depend on an identity field. `VRFConfig` is a unit keyed by `Name`:
- `user: {name: "a"}` + `cloud: {name: "a", table: 100}` -> `{name: "a", table: 100}` (matching domain keeps explicit table).
- `user: {name: "b"}` + `cloud: {name: "a", table: 100}` -> `{name: "b", table: nil}` -> Stage 4 derives `TableIDForName("b")` (no foreign table leakage).
- `user: {table: 200}` + `cloud: {name: "a", table: 100}` -> `{name: "a", table: 200}` (table-only override works).

Future identity-bearing structs must follow this same rule and document their identity key here.

---

## 4. Reference Semantics and Deep Copy

`MergeNetworkConfig`, `MergeNetworkConfigs`, and `(*NetworkConfig).Clone` guarantee that their return value shares **no** pointers, maps, or overlapping slice backing arrays (including spare capacity) with any input—including inside slice elements and map values.

- **Current strategy:** API types are not restricted to value-only slice elements or map values. Today `RouteConfig`, `RuleConfig`, and `NeighborConfig` contain only values, so `slices.Clone` is a deep copy. If a future field adds a pointer, slice, or map inside a slice element or map value, the reflection tests in `pkg/apis/config_merge_coverage_test.go` fail immediately until explicit element cloning is added to `MergeNetworkConfig` and `Clone`.
- **When to adopt `deepcopy-gen`:** Only if nested reference types grow numerous enough that explicit copy helpers become a maintenance burden.

---

## 5. Enforcement Gates and New API Field Checklist

Every PR that adds or modifies a field on `NetworkConfig` (or any nested type) must satisfy these automated checks (`make test` runs with `-race`):

1. **Type definition (`pkg/apis/types.go`):**
   - Use pointers for optional scalars where zero (`0`, `false`, `""`) is a meaningful override.
   - Document whether the field is part of a unit type, has a discriminator dependency, or has a fallback default.
2. **Merge and Clone (`pkg/apis/config_merge.go`):**
   - Merge the field explicitly by name in `MergeNetworkConfig` (or its nested helper) and copy it in `Clone()`.
   - **Enforced automatically by** `pkg/apis/config_merge_coverage_test.go`:
     - `TestMergeKeepsEveryFieldFromEitherSide` & `TestMergeKeepsEachFieldAlone` (field survival and copy-paste detection across merge and `Clone`)
     - `TestMergeKeepsEmptyStructPointersAndDoesNotAlias` (empty struct pointers / presence markers)
     - `TestMergeUserWinsEveryField`, `TestMergeMapKeyConflictsUserWins`, `TestMergeEmptyNonNilMapsDoNotAlias`, & `TestMergeUserZeroPointerWinsEveryField` (override, map key conflict, empty map isolation, and zero-value pointer semantics)
     - `TestMergeSharesNoMemoryWithInputs` & `TestClonePreservesAndIsolatesAllFields` (recursive shared-memory check into slice backing capacity, slice elements, and map values)
3. **Normalization, Defaulting, and Validation (`pkg/apis/defaults.go`, `pkg/apis/validation.go`):**
   - Put deprecated-alias folding in `Normalize()` (non-mutating).
   - Put fallback derivation in `Default()` (runs once at Stage 4).
   - Put single-source checks in intake (`validationModeIntake`) and completeness/cross-field checks in final validation (`validationModeFinal`).
   - Update `ValidateRDMAOnlyConfig` if the field is an interface-only network setting (**enforced automatically by** `TestValidateRDMAOnlyConfigRejectsEveryInterfaceField` in `pkg/apis/validation_test.go`).
4. **Driver Pipeline Contract (`pkg/driver/dra_hooks_test.go`):**
   - `TestResolveDeviceNetworkConfigContract` verifies precedence, cloned non-defaulted `GetProfileConfig` input, VRF cases A/B/C, `dhcp` normalization across sources, rollback on Stage 3 profile intake and Stage 4 final validation failures, nil-callback/claim safety, and concurrent `-race` safety against cached device configs.
5. **Checkpoint Wire Format (`pkg/driver/pod_device_config_bolt_version_test.go`):**
   - Populate the field in `fullDeviceConfig()` and update `goldenDeviceConfigJSON` (**enforced automatically by** recursive `TestDeviceConfigFixturePopulated`).
