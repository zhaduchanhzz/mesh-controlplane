# PR P00d Evidence — source inventory: netmap tăng dần sau công tắc mặc định OFF

- **Trạng thái:** `Planned`. File này chỉ là inventory: chưa đổi code, chưa chạy test. Các mục đánh dấu **[Đề xuất]** là đề xuất chờ chủ dự án chốt, **chưa** phải quyết định.
- **Nhánh:** `feat/P00d-incremental-netmap`.
- **Ticket / ADR:** `tickets/P00d.md`; ADR 0005 §"Netmap tăng dần" (biện pháp 1–5) và quyết định 3; ADR 0007 §1 (runtime theo tailnet); `06-tests-benchmarks.md` §1c (M01–M06); PLAN dòng 72, 272, 278, 290–291.

## 1. Repo commit before / after

| | Commit |
|---|---|
| Before (inventory đọc trên) | `3269688e` (= `master`, đã có merge P00c `37f8f2c`: `BuildPeerMap` có index, NodeStore flush khi rebuild và nhả writer theo thứ tự hàng đợi) |
| After | chưa có (chưa có commit code) |

Đọc bằng `git`/`grep`/`sed` trên working tree sạch tại `3269688`. Không chạy `go test`, không chạm lab.

## 2. Path & symbol đã đọc (file:line tại `3269688`)

### 2.1 `hscontrol/state/node_store.go` — snapshot và writer

| Symbol | Dòng | Ghi chú |
|---|---|---|
| hằng `put`/`del`/`rebuildPeerMaps`/`setName`/`updateMulti` | 40–46 | 5 loại work; không có op riêng cho reconcile |
| metric `nodeStore*`, `nodeStoreSnapshotBuilds{peers}` | 48–103 | `promauto`, namespace `headscale`, biến package-level (dùng chung cho mọi runtime trong process). `nodestore_snapshot_builds_total{peers="recomputed"\|"reused"}` là hook có sẵn cho M02 |
| `NodeStore` (`data atomic.Pointer[Snapshot]`, `peersFunc`, `writeQueue`, `batchSize`, `batchTimeout`) | 116–133 | một writer goroutine; đọc không khóa |
| `NewNodeStore` | 135–152 | snapshot đầu tiên luôn full (`snapshotFromNodes(..., nil, false, false)`) |
| `Snapshot` | 200–221 | `nodesByID`, `nodeViewsByID`, `nodesByNodeKey`, `nodesByMachineKey`, **`peersByNode map[NodeID][]NodeID`** (adjacency theo ID, bất biến sau publish), `nodesByUser`, `allNodes`, `routes`, `isPrimaryRoute` |
| `PeersFunc` | 227 | `func([]NodeView) map[NodeID][]NodeID` — chỉ có API "cả tập", không có API một node |
| `work` | 230–251 | `del` có `peers *[]NodeID` để trả peer **trong snapshot trước** |
| `updateChanges(pre, post)` | 259–270 | relation = `NodeView.HasPolicyChange`; election = online/expiry/unhealthy/SessionStart |
| `PutNode` / `UpdateNode` / `UpdateNodeDiff` / `UpdateNodes` / `DeleteNode` / `SetGivenName` | 277–485 | mọi write chặn tới khi batch được áp dụng |
| `processWrite` | 503–539 | flush khi đủ `batchSize`, hết `batchTimeout`, hoặc **ngay khi gặp `rebuildPeerMaps`** (P00c) |
| `applyBatch` | 551–701 | xem §3.1 |
| `snapshotFromNodes(nodes, peersFunc, prev, reusePeers, reuseRoutes)` | 764–843 | `allNodes` dựng bằng cách duyệt **map** → thứ tự ngẫu nhiên mỗi snapshot. `reusePeers` → `prev.peersByNode`; ngược lại `peersFunc(allNodes)` toàn bộ |
| `ListPeers(id)` | 1072–1089 | resolve `peersByNode[id]` qua `nodeViewsByID` của **snapshot hiện tại** (ID đã xóa tự rơi ra) |
| `ListPeerIDs` | 1093–1095 | bản sao đã sort |
| `RebuildPeerMaps` | 1206–1221 | gửi work `rebuildPeerMaps`, chờ `rebuildResult` |

### 2.2 `hscontrol/state/state.go` — nối dây và nơi phát change

| Symbol | Dòng | Ghi chú |
|---|---|---|
| `NewStateWithDB` (đoạn dựng `polMan`, `NewNodeStore(nodes, policyPeersFunc(polMan), ...)`) | ~438–455 | mỗi `State` có `PolicyManager` và `NodeStore` riêng |
| `policyOptions` | 505–507 | `WithEmptyPolicyDenies(cfg.PolicyEmptyDenies)` |
| `policyPeersFunc(pm)` | 510–523 | **gọi `pm.SetNodes(slice)` rồi `pm.BuildPeerMap(slice)` trên writer goroutine**. SetNodes có thể biên dịch lại filter ngay trong lúc dựng adjacency |
| `ReloadPolicy` / `reloadPolicyLocked` | 590–653 | policy đổi → `ClearSSHCheckAuth` + `RebuildPeerMaps`; trả `PolicyChange` (+ `DERPMap`) |
| `CreateUser` | 656–681 | `updatePolicyManagerUsers`; rỗng thì vẫn `PolicyChange` |
| `UpdateUser` / `DeleteUser` / `RenameUser` | 684–846 | `DeleteUser` → `UserRemoved()` (= `FullUpdate` cho mọi node) nếu policy không đổi. `db.DestroyUser` (`hscontrol/db/users.go:53`) **từ chối** khi user còn node: "xóa user" của M06 thực chất là xóa node trước |
| `SaveNode`, `persistNodeAndRefreshPolicy` | 940–970 | |
| `DeleteNode` | 973–1010 | `peers := nodeStore.DeleteNode(id)` (adjacency **cũ**) → `change.NodeRemoved(id, peers...)` + `updatePolicyManagerNodes` |
| `Connect` / `Disconnect` | 1017–1147 | patch online/offline; `PolicyChange` chỉ khi `NodeNeedsPeerRecompute` (router, relay, via) |
| `ListPeers(nodeID, peerIDs...)` | 1261–1287 | lọc `PeersChanged` theo adjacency của **người nhận** |
| `updateNodeExpiry` | 1381–1453 | `NodeAdded` + policy change nếu có; expiry **không** phải input relation |
| `SetNodeTags` | 1458–1540 | write relation → thường ra `PolicyChange`; rỗng thì `NodeAdded` |
| `SetApprovedRoutes` / `updateApprovedRoutes` | 1545–1600 | `PolicyChange` khi primary đổi hoặc policy đổi |
| `RenameNode` | 1605–1650 | `NodeAdded` |
| `ExpireExpiredNodes` | 1742–1790 | `UpdateNodes` một batch, `NodeAdded` mỗi node |
| `SetPolicy` | 1849–1862 | `pm.SetPolicy` rồi `RebuildPeerMaps` (hai bước, không nguyên tử với writer — xem rủi ro R4) |
| `UpdatePolicyIf` | 1979–2025 | qua `reloadPolicyLocked` |
| `HandleNodeFromPreAuthKey` (cuối) | 3740–3760 | `policyChanged := !usersChange.IsEmpty() \|\| !nodesChange.IsEmpty()` → `reauthChange(...)` |
| `reauthChange` | 4096–4107 | `policyChanged` → **`PolicyChange()`**; không thì `NodeKeyRotated`/`NodeAdded` |
| `updatePolicyManagerUsers` | 4147–4176 | `SetUsers` → `peerMapChanged` → `RebuildPeerMaps` |
| `DrainSelfRefreshes` | 4191–4207 | |
| `updatePolicyManagerNodes(genBefore)` | 4219–4253 | `NodesGeneration() != genBefore` → `PolicyChange` |
| `policyChangeSince` | 4267–4272 | |

### 2.3 `hscontrol/policy/v2/policy.go`, `peermap_index.go`, `compiled.go`

| Symbol | Dòng | Ghi chú |
|---|---|---|
| `PolicyManager` (`matchers`, `filterHash`, `compiledGrants`, `userNodeIdx`, `needsPerNodeFilter`, `filterRulesMap`, `matchersForNodeMap`, `sshPolicyMap`, `nodesGen`, `denyAll`, `emptyDenies`, ...) | 37–130 | `mu sync.RWMutex` |
| `updateLocked` | 315–500 | biên dịch lại toàn bộ grant, tag owner, autoapprover, exit set, nodeAttrs; `pm.matchers` **chỉ dựng lại khi `filterHash` đổi** (407–410). `needsUpdate = filter \|\| tagOwner \|\| autoApprove \|\| exitSet` changed → xóa cache SSH/filter/matcher |
| `denyAll := emptyDenies && hasNoAccessRules(pol)` | 372–373 | |
| `NodeNeedsPeerRecompute` | 514–534 | |
| `SetPolicy` | 749– | |
| `BuildPeerMap` | 885–894 | `RLock` → `buildPeerMapIndexedLocked` |
| `buildPeerMapPairwiseLocked` | 902–1074 | bản tham chiếu; `denyAll` → map rỗng (905–907); đường global (bitset) và đường per-node (`nodeMatchersLocked` + `CanAccessWithRoutes` 4 chiều) |
| `addCapSourceOnExit` | 1079–1087 | quan hệ **một chiều**: exit node liệt kê nguồn capability tới internet |
| `matcherReachesSource` / `matcherReachesDestination` | 1089–1101 | |
| `sameUser` | 1114–1123 | node của cùng user luôn là peer (trừ tagged); bị `denyAll` chặn |
| `nodeMatchersLocked` | 1140–1146 | `pm.matchers` + `nodeOnlyMatchers(...)` (`compiled.go:799`) |
| `SetUsers` | 1244–1293 | luôn xóa cache per-node; trả `(policyChanged, peerMapChanged=true, err)` khi danh sách user đổi |
| `SetNodes` | 1314–1373 | `nodesHavePolicyAffectingChanges` (1408–1427) đúng khi **số node khác** hoặc một node `HasPolicyChange`; khi đó `updateLocked`, **`nodesGen++` và trả `true` kể cả khi filter không đổi** (1363–1369) |
| `NodesGeneration` | 1378–1384 | |
| `invalidateAutogroupSelfCache` / `invalidateNodeCache` / `invalidateGlobalPolicyCache` | 2119–2280 | cache per-node xóa chọn lọc theo user bị ảnh hưởng (per-node) hoặc theo node đổi (global) |
| `peermap_index.go` `buildPeerMapIndexedLocked` | 38–258 | 4 quy tắc: (1) cặp lớp matcher global, (2) matcher riêng node qua `peerIndex`, (3) same-user, (4) exit liệt kê capSources. Fallback pair scan khi view không hợp lệ hoặc ID trùng (48–62) |
| `peerNode`, `peerClass`, `bitsIntersect`, `peerIndex`, `newPeerIndex`, `sources`, `destinations`, `collect` | 261–418 | tất cả là cấu trúc **tạm** dựng trong mỗi lần build, không giữ lại giữa các build |
| `types/node.go` `NodeView.CanAccess` / `CanAccessWithRoutes` | 1011, 1022 | hàm từng cặp có sẵn (A có tới được B với tập matcher cho trước) |
| `types/node.go` `NodeView.HasPolicyChange` | 1338–1370 | user, user association, tags, IPs, subnet routes, exit routes |
| `policy/pm.go` `PolicyManager` interface | 14– | không có API "peer của một node" |
| `policy/v2/types.go` `Asterix.resolve` | 394–396 | `*` → hằng `asterixResolved()`, **không** phụ thuộc node |

### 2.4 `hscontrol/types/change/change.go`

| Symbol | Dòng | Ghi chú |
|---|---|---|
| `Change` (`TargetNode`, `OriginNode`, `Include*`, `PeersChanged`, `PeersRemoved`, `PeerPatches`, `SendAllPeers`, `DeletedNodes`, `RequiresRuntimePeerComputation`, `PingRequest`) | 19–57 | |
| `Merge` | 74–131 | panic khi hai `TargetNode` khác nhau |
| `IsFull` | 167–170 | **không xét `TargetNode`** |
| `HasFull`, `CollapseToFull` | 236–256 | `CollapseToFull` trả `FullUpdate()` (không target) + ping |
| `DedupePolicyChanges` | 298–319 | bỏ `PolicyChange` broadcast lặp trong một tick |
| `isPeerDelta`, `isDeletion`, `CoalescePeerDeltas` | 323–408 | xóa node gộp thành **một** removal đặt lên đầu tick; không bỏ removal |
| `FullUpdate`, `FullSelf`, `PolicyChange`, `NodeAdded`, `NodeRemoved`, `UserAdded`, `UserRemoved` | 422–670 | `NodeRemoved(id, peers...)`: 1 change teardown target `id` + 1 change target mỗi peer (`PeersRemoved`+`DeletedNodes`) |

### 2.5 `hscontrol/mapper/` — fan-out và backpressure

| Symbol | Dòng | Ghi chú |
|---|---|---|
| `mapResponseGenerated{response_type}`, `mapperChangesDropped{reason}` | `batcher.go:35–47` | mẫu metric label bị chặn tập giá trị |
| `NewBatcherAndMapper` | `batcher.go:63–69` | một batcher + mapper cho mỗi `State` |
| `generateMapResponse` | `batcher.go:84–170` | `RequiresRuntimePeerComputation` → `ListPeers` + `computePeerDiff` + `policyChangeResponse`; có `resp.Peers` → `computePeerDiff`; `withRemovals` đặt removal thành frame riêng đứng trước |
| `handleNodeChange` | `batcher.go:209–244` | `updateSentPeers` chỉ sau khi gửi được |
| `AddNode` | `batcher.go:306–480` | initial map = `FullSelf(id)` qua `MapResponseFromChange`; xử lý removal khi node không có stream; dùng `nodeConn.collapsePendingToFull(nil)` để ép full **cho riêng node đó** (~405) |
| `AddWork` | `batcher.go:519–531` | bỏ change rỗng |
| `worker` | `batcher.go:600–720` | `errNoReadyConnections`/`errConnectionBackpressure` → `prependPending(retry...)` |
| `addToBatch` | `batcher.go:736–807` | `DeletedNodes` → đóng session node bị xóa; `HasFull` → **mọi** node `collapsePendingToFull`; targeted → `appendPending` node đó; broadcast → `appendPending` **mọi** node trong `b.nodes` |
| `processBatchedChanges` | `batcher.go:810–840` | `CoalescePeerDeltas(DedupePolicyChanges(pending))`, một work/node/tick, `inFlight` |
| `mapper.policyChangeResponse` | `mapper.go:316–338` | PacketFilters + SSHPolicy + Self + **toàn bộ peer hiện tại trong `PeersChanged`** |
| `mapper.buildFromChange` | `mapper.go:342–418` | `PeersChanged` → `state.ListPeers(nodeID, ids...)` (lọc theo adjacency người nhận); `PeersRemoved` gửi nguyên |
| `filterVisiblePeerPatches` | `mapper.go:425–445` | patch chỉ cho peer nhìn thấy |
| `builder.buildTailPeers` | `builder.go:294–390` | **sort peer theo ID** (383): thứ tự trong `peersByNode` không ảnh hưởng MapResponse |
| `minPendingFold=64`, `connectionStallTimeout=5s`, `connectionSendWait=50ms`, `maxPendingSize=256` | `node_conn.go:24–58` | |
| `appendPending`, `prependPending`, `collapsePendingToFull`, `takePending` | `node_conn.go:360–457` | vượt `maxPendingSize` → `CollapseToFull`; node không có connection → pending bị bỏ (`no_connection`) |
| `multiChannelNodeConn.send`, `connectionEntry.send` | `node_conn.go:467–656` | chờ 50 ms rồi backpressure; kẹt ≥ 5 s → `ErrConnectionSendTimeout`, connection bị gỡ, session bị dừng |
| `updateSentPeers`, `computePeerDiff` | `node_conn.go:679–730` | `lastSentPeers` + `partialPeers`: những gì client **đã nhận** |

### 2.6 Cấu hình, metrics, runtime

| Path | Symbol / dòng | Ghi chú |
|---|---|---|
| `hscontrol/types/config.go` | `Tuning` 993–1027; `//go:generate` dòng 3–4 (`tools/configgen`) | key `tuning.*`, tag `koanf`/`example`/`defaulttext` |
| `hscontrol/types/config_docs_generated.go` | sinh từ doc comment | không sửa tay (chạy `go generate`) |
| `hscontrol/types/config_fields_test.go` | `TestConfigFields` — danh sách viết tay, `tuning.*` ở 145–151, section `tuning` ở 241 | key/section mới phải thêm vào đây |
| `hscontrol/config/default.go` | `Tuning{...}` 83–90 | default |
| `hscontrol/config/validate.go` | ~219–222 | mẫu kiểm tra giá trị dương |
| `docs/ref/config-example.yaml`, `docs/ref/configuration-options.md` | ~494–499, ~3548–3593 | sinh bởi `tools/configgen/files` (giả định, xem §7 A6) |
| `hscontrol/app.go` | `newHeadscale` 209, `scheduledTasks` 384–500, `Change` 1391–1401, `startRuntime` 1533–1611 (`NewBatcherAndMapper` 1542, `go h.scheduledTasks` 1611) | mọi thứ per `Headscale` |
| `hscontrol/cluster.go` | `tenantRuntime` 105–117, `NewCluster` 122–185 (`cfg.PolicyEmptyDenies = true`), `load` 211–257 (`newHeadscale(... state.NewStateWithDB(c.cfg, tdb))`), `startRuntime` 268 | xem §3.6 |

### 2.7 Test có sẵn dùng làm harness

| Test | Path:dòng | Dùng cho |
|---|---|---|
| `TestBuildPeerMapIndexedMatchesPairwise` (rapid, mặc định 2000 checks, coverage self/via/exitCap/denyAll) + `drawPeerMapNodes`, `drawPeerMapPolicy`, `pairwisePeerMap` | `policy/v2/peermap_index_test.go:20–300` | M01 mức policy: so API một-node với full build |
| `TestNodeStoreAdjacencyMatchesFullBuild` (rapid, NodeStore thật + PolicyManager thật, oracle `checkAdjacencyMatchesFullBuild` dùng **PolicyManager mới tạo**) + `nodeStoreWithPolicy`, `syncPolicy`, `policyGlobal/AutogroupSelf/Via` | `state/node_store_test.go:1781–1980` | M01 mức NodeStore: chỉ cần mở rộng op (join/leave/user/policy/expiry) và bật công tắc |
| `TestUpdateChangesCoversEveryNodeField`, `nodeFieldImpact` | `state/node_store_test.go:1953–1999` | field mới của `Node` phải phân loại |
| `TestPeerIrrelevantWriteReusesPeerMap`, `TestUpdateNodeRecomputesPeersOnlyForRelationInputs`, `TestStatePolicyWriteBuildsPeersOnce` | `state/node_store_test.go:1380, 1585, 2255` | M02 (đếm số lần build) |
| `TestRebuildPeerMapsAppliesQueuedWritesFirst` | `state/node_store_rebuild_test.go:16` | thứ tự nhả writer khi có op mới |
| `TestPendingBacklogCollapsesToCurrentState`, `TestBatcherSlowConsumerRetriesInOrder`, `TestBatcherStalledConsumerEvicted`, `TestHandleNodeChangeSendsRemovalsAsDelta`, `TestAddNodeRemovalSurvivesPendingLoss`, `TestNodeDeletedWhileChangesPending` | `mapper/batcher_unit_test.go:430, 478, 549`, `mapper/batcher_test.go:2886, 2973, 2477` | M04 |
| `TestAppendPendingStaysBoundedWhileInFlight`, `TestAppendPendingBoundsFoldedPatches` | `mapper/batch_coalesce_test.go:79, 119` | M04 (hàng đợi đầy) |
| `servertest` (`TestScale`, cluster P05 `-tags p05cluster`) | `hscontrol/servertest/` | M05 có thể ghi chuỗi MapResponse từ client in-process |
| `pgregory.net/rapid v1.3.0` | `go.mod:89` | **đã có**, không cần thêm dependency |

**Không có golden test chuỗi MapResponse** trong `hscontrol/mapper` (chỉ có golden ở config/policy compat). M05 phải dựng harness mới.

## 3. Phát hiện

### 3.1 Cái gì kích hoạt full recompute hôm nay

`applyBatch` (551–701) đặt `relationChanged = true` cho **mọi** `put`, `del`, `setName`, `rebuildPeerMaps`, và cho `updateMulti` khi `updateChanges` thấy `HasPolicyChange`. `relationChanged` → `snapshotFromNodes(..., reusePeers=false)` → `peersFunc(allNodes)` = `pm.SetNodes` + `pm.BuildPeerMap` cho **toàn bộ** tailnet. Không có đường trung gian.

"Reuse" chỉ có một dạng: batch không có thay đổi relation (online/offline, endpoint, hostinfo không đổi route, expiry, health, ...) → giữ nguyên `prev.peersByNode` (và `prev.routes` nếu không có input election). `ListPeers` resolve ID qua view mới nên payload không cũ.

Nhận xét:
- `setName` bị tính là relation (640–641) dù `GivenName` không nằm trong `HasPolicyChange`. Full rebuild thừa, không ảnh hưởng an toàn.
- `put` luôn là relation, kể cả khi `PutNode` ghi lại node không đổi input policy.
- Các cấu trúc của build có index (`peerClass`, `peerIndex`, bitset) **không được giữ** giữa các snapshot. Muốn tính tăng dần thì phải lưu chúng (hoặc dựng lại phần cần thiết).

### 3.2 Biên "policy change" và "topology change" bị nhòe ở tầng PolicyManager

Matcher là **địa chỉ IP** đã resolve: `user@`, group, tag, `autogroup:member`, `autogroup:self` đều resolve sang IP của node. Vì vậy:
- Node join/leave hoặc đổi tag/user/IP/route → `SetNodes` (được gọi **bên trong** `policyPeersFunc` trên writer) → `nodesHavePolicyAffectingChanges` đúng (số node đổi là đủ) → `updateLocked` biên dịch lại filter. Nếu policy nhắc tới user/group/tag của node đó thì `filterHash` đổi, `pm.matchers` đổi, **lớp matcher của node khác cũng có thể đổi**. Thực chất đây là policy change do topology gây ra.
- Chỉ khi policy chỉ dùng `*` (hằng, `types.go:394`) hoặc node không nằm trong alias nào thì join mới để nguyên `filterHash`.
- Với `needsPerNodeFilter` (autogroup:self, via), filter global gần như rỗng nhưng matcher riêng node của **mọi node cùng user** đổi khi user đó thêm/bớt node (`invalidateAutogroupSelfCache`).
- `SetNodes` trả `true` và tăng `nodesGen` **mỗi khi tập node đổi**, kể cả khi `updateLocked` báo không có gì đổi (1356–1369). Interface hiện tại không cho biết *cái gì* đã đổi.

Hệ quả cho biện pháp 2: muốn "đổi policy thì luôn full rebuild" đúng nghĩa, đường tăng dần chỉ được dùng khi writer **biết chắc** lần `SetNodes` của batch không đổi `pm.matchers`, không đổi matcher riêng node của node nào khác, và không đổi `denyAll`. Cần API mới trả chi tiết thay đổi; không suy ra được từ `bool` hiện có.

### 3.3 Có sẵn phép tính peer theo từng node không?

Không có API "peer của node X". Có các mảnh dùng lại được:
- `NodeView.CanAccessWithRoutes(matchers, other, ...)` (`types/node.go:1022`): kiểm một chiều, từng cặp, với tập matcher cho trước. Dùng nó cho X so với n node là O(n·|matchers|): đúng nhưng chậm, giống pair scan.
- Phía global của `buildPeerMapIndexedLocked`: bitset src/dst của X với `pm.matchers` tính trong O(|matchers|); X linked với lớp C khi `bitsIntersect`. Nếu giữ được danh sách lớp (bitset → members) từ lần build trước **và matchers không đổi**, peer global của X = hợp members của các lớp linked, cộng same-user (`byUser`), cộng `capSources` nếu X là exit, cộng X vào list của exit node nếu X là capSrc (quan hệ một chiều, `addCapSourceOnExit`).
- Phía per-node (quy tắc 2): `nodeOnlyMatchers(compiledGrants, X, userNodeIdx, viaPeerRule)` + `peerIndex.sources/destinations`. Nhưng thêm/bớt X đổi matcher self của **các node cùng user**, nên phải tính lại cả các node đó, không chỉ X.

Cache matcher: `pm.matchers` thay khi `filterHash` đổi (`updateLocked` 407–410). `filterRulesMap`, `matchersForNodeMap`, `sshPolicyMap` bị xóa khi `needsUpdate`, khi `SetUsers` (luôn), khi `SetNodes` có thay đổi mà `updateLocked` không đổi (1356–1362), và xóa chọn lọc trong `invalidateNodeCache`. Đổi user → `SetUsers` → `peerMapChanged=true` → `RebuildPeerMaps` (full). Đổi tag của node → `HasPolicyChange` → relation → full.

### 3.4 Fan-out hôm nay: ai nhận delta, ai nhận full

- Node **join** (đăng ký mới): `SetNodes` luôn báo đổi → `nodesGen` tăng → `updatePolicyManagerNodes` trả `PolicyChange` → `reauthChange` trả **`PolicyChange()`** (broadcast, `RequiresRuntimePeerComputation`). Mỗi node đang kết nối dựng `policyChangeResponse`: PacketFilters + SSHPolicy + Self + **toàn bộ peer hiện tại trong `PeersChanged`**. Đây đúng là hot spot §7 mục 4–5 của P00b: một join = n MapResponse, mỗi cái O(peer) → O(n²) byte ở allow_all.
- `NodeAdded(id)` (broadcast `PeersChanged=[id]`): mọi node trong `b.nodes` nhận pending; lúc render, `State.ListPeers(R, id)` lọc theo adjacency của R; R không thấy id thì response rỗng → `emptyMapResponse` bỏ. Chi phí O(n) lần render nhỏ, không có frame.
- Node **xóa**: `NodeRemoved(id, peersCũ...)` **targeted** tới từng peer trong adjacency cũ (`PeersRemoved`+`DeletedNodes`), cộng `PolicyChange` broadcast từ `updatePolicyManagerNodes`.
- User thêm/xóa: `UserAdded`/`UserRemoved` = `FullUpdate` → `addToBatch` ép **mọi** node `collapsePendingToFull`.
- Policy change: `PolicyChange` broadcast.
- Online/offline node thường: patch (`NodeOnline`/`NodeOffline`) broadcast, lọc theo visibility ở render.

Cơ chế thu hồi hiện tại dựa vào **broadcast `PolicyChange` + `computePeerDiff`** (so peer hiện tại với `lastSentPeers`/`partialPeers` — những gì client đã nhận). `PeersChanged` một mình **không** sinh removal: nếu R mất quyền thấy X, `ListPeers(R, X)` lọc X ra và R không nhận gì. Hôm nay an toàn vì mọi thay đổi relation đều kéo theo `PolicyChange` (cùng predicate `HasPolicyChange` ở `updateChanges` và `nodesHavePolicyAffectingChanges`).

### 3.5 Delta thu hồi/xóa có bị bỏ hoặc gộp không?

Hôm nay **không bị bỏ im lặng**, nhưng có các đường suy biến:
- Gộp: `CoalescePeerDeltas` gộp mọi deletion của tick thành một removal đặt lên đầu; không mất ID. `DedupePolicyChanges` chỉ bỏ `PolicyChange` broadcast trùng trong tick (tương đương nhau).
- Hàng đợi đầy: `appendPending`/`prependPending` vượt `maxPendingSize=256` → `CollapseToFull` → full map render trạng thái hiện tại; `computePeerDiff` sinh `PeersRemoved` riêng (`withRemovals`) vì client bỏ qua `Peers` rỗng.
- Backpressure (kênh connection đầy 50 ms): phần chưa gửi được `prependPending` lại, giữ thứ tự.
- Connection kẹt ≥ 5 s: connection bị gỡ, session dừng; client reconnect nhận full map, `AddNode` gửi removal còn thiếu (`TestAddNodeRemovalSurvivesPendingLoss`).
- Node không có connection lúc `takePending`: pending **bị bỏ** (`mapper_changes_dropped_total{reason="no_connection"}`); an toàn vì lần kết nối sau mở bằng full map + removal.
- Điểm yếu: target của `NodeRemoved` lấy từ **adjacency cũ** (`prev.peersByNode`). Nếu adjacency cũ sai (thiếu R) thì R không nhận removal targeted; hôm nay `PolicyChange` broadcast đi kèm che lỗ này. Nếu đường tăng dần bỏ broadcast đó, lỗ này lộ ra.
- Không có full map định kỳ nào hôm nay (không tìm thấy ticker gửi `FullUpdate`).

### 3.6 Đa tenant (ADR 0007, P05)

`Cluster.load` (`cluster.go:211–257`) gọi `newHeadscale(... state.NewStateWithDB(c.cfg, tdb))` cho từng tailnet; `startRuntime` (`app.go:1533`) tạo `NewBatcherAndMapper(h.cfg, h.state)` và `scheduledTasks` cho từng `Headscale`. Vì vậy `State`, `NodeStore` (snapshot, writer, `peersFunc`), `PolicyManager` (matcher, cache, `nodesGen`) và `Batcher` (pending, `lastSentPeers`) đều **riêng từng runtime**. Thứ dùng chung trong process:
- `*types.Config` (`c.cfg`): công tắc mới chỉ đọc, an toàn khi dùng chung; nhưng mọi tenant cùng bật/tắt.
- Biến metric `promauto` package-level: số đếm cộng dồn mọi runtime, không có label tenant (đúng yêu cầu ticket; nhưng không phân biệt được runtime nào lệch, phải dựa vào log đã redact).

Không thấy state nào trên đường peer map/fan-out dùng chung giữa runtime. Đường tăng dần đặt trong `NodeStore`/`PolicyManager`/`Batcher` sẽ tự nhiên per-runtime. P07 vẫn phải chạy lại M01/M03 với nhiều tenant.

## 4. Constraints hiện có

- AGENTS 6–9, 13: mọi lệch isolation là BLOCK MERGE; tenant policy rỗng = DENY (`WithEmptyPolicyDenies`, cluster ép `PolicyEmptyDenies = true`); metric không lộ node/IP/key và chạy trên `metrics_listen_addr` riêng.
- Ticket: công tắc OFF phải giữ nguyên hành vi P00c (M05); không đổi giao thức hoặc nội dung MapResponse ngoài việc chọn người nhận.
- `PolicyManager` lock được `policyPeersFunc` lấy trên writer goroutine: PolicyManager không bao giờ được chờ một NodeStore write khi đang giữ lock (comment `state.go:493–503`). Op mới chạy trên writer cũng phải giữ quy tắc này.
- `applyBatch` nhả writer theo thứ tự hàng đợi (P00c); op mới phải giữ thứ tự đó.
- `TestUpdateChangesCoversEveryNodeField` buộc phân loại field mới.
- Config: thêm key thì cập nhật `TestConfigFields`, `default.go`, validate, rồi `go generate` cho docs; không sửa tay file generated.

## 5. Đề xuất và quyết định

**Quyết định của chủ dự án (2026-11-23), áp dụng cho §5.1–§5.3:**
- **Q1 — chấp nhận:** khi filter đã biên dịch được chứng minh không đổi, đường tăng dần gửi `NodeAdded(X)` (delta một peer) thay cho `PolicyChange` khi node join. Coi là trong phạm vi: cùng các trường MapResponse, chỉ là loại change hẹp hơn.
- **Q2 — chấp nhận §5.1:** `tuning.incremental_netmap.{enabled=false, reconcile_interval=60s, full_map_interval=15m}`.
- **Q3 — chấp nhận §5.2:** M06 ≤ 10 s ở mọi lần thử, qua đường delta/policy (hai interval đặt 1 h khi chạy M06).
- **Q4 — chấp nhận §5.3:** bước đầu chỉ join node mới và xóa node, phạm vi hẹp; autogroup:self, via, route/exit/capability để ticket sau. Mọi trường hợp khác full rebuild.


### 5.1 [Đề xuất] Tên và giá trị mặc định của công tắc

Section mới dưới `tuning` (cùng chỗ với các tham số NodeStore/batcher):

| Key | Kiểu | Mặc định | Ràng buộc |
|---|---|---|---|
| `tuning.incremental_netmap.enabled` | bool | `false` | |
| `tuning.incremental_netmap.reconcile_interval` | duration | `60s` | chỉ đọc khi `enabled`; ≥ `10s` |
| `tuning.incremental_netmap.full_map_interval` | duration | `15m` | chỉ đọc khi `enabled`; ≥ `1m`; trải đều theo node (mỗi node một lần trong chu kỳ, không gửi cả tailnet cùng lúc) |

Struct Go gợi ý: `Tuning.IncrementalNetmap IncrementalNetmapConfig` (`Enabled`, `ReconcileInterval`, `FullMapInterval`). Section mới làm `TestConfigFields` phải thêm `tuning.incremental_netmap` vào danh sách section và 3 leaf.

Lý do 60 s: đối chiếu là một `BuildPeerMap` đầy đủ; sau P00c chưa có số microbench ở 10k (§7 P00c ghi chưa chạy lại `BenchmarkCapacity_*`). Giá trị cần đo lại rồi chốt. Lý do 15 m: một full map cho mọi node ở 10k allow_all là O(n²) byte, đúng thứ P00d muốn tránh; trải đều thì mỗi tick chỉ ~n/(15·60/0.8) node.

Metric (không label tenant, không label tự do):
- `headscale_nodestore_netmap_divergence_total` (counter, không label, hoặc label `kind` ∈ {`extra`,`missing`}; `extra` = tăng dần cho thấy peer mà full build không cho, tức nghi lỗi isolation).
- `headscale_nodestore_netmap_reconcile_duration_seconds` (histogram).
- Thêm giá trị `incremental` cho `nodestore_snapshot_builds_total{peers}` (hook cho M02).

### 5.2 [Đề xuất] Giới hạn M06

- Peer phải biến mất khỏi `tailscale status --json` của client thật trong **≤ 10 s** sau khi API trả về (thu hồi node, xóa node rồi xóa user, đổi policy chặn), **ở mọi lần thử**; ghi p50/max. Với mặc định (`batch_change_delay` 800 ms, `node_store_batch_timeout` 500 ms) đường delta kỳ vọng < 3 s; 10 s chừa cho lab và poll mỗi 1 s như `infra/lab/e2e/p05/run.sh` (`wait_*` loop).
- Kèm điều kiện: removal phải đến bằng đường delta/policy, **không** nhờ full map định kỳ hay đối chiếu (đặt `full_map_interval` và `reconcile_interval` lớn hơn nhiều so với 10 s trong lần chạy M06, ví dụ 1 h).
- Mỗi kịch bản ≥ 5 lần, công tắc ON và OFF, ghi phiên bản client.

### 5.3 [Đề xuất] Phạm vi tăng dần đầu tiên (hẹp nhất)

Chỉ một loại: **node join và node leave (xóa)**, khi **đồng thời**:
1. công tắc ON;
2. batch chỉ gồm `put` của node **mới** (chưa có trong `prev.nodesByID`) và/hoặc `del`; không có `updateMulti` mang thay đổi relation, không `setName`, không `rebuildPeerMaps`;
3. lần `SetNodes` của batch **không** đổi `filterHash`, tagOwner, autoApprove, exitSet, nodeAttrs, relayTargetIPs, viaTargetTags, SSH rules, `denyAll` (cần API mới trả chi tiết, §3.2);
4. `!needsPerNodeFilter` (không autogroup:self, không via);
5. node join/leave không có approved subnet/exit route và không là capSrc (tránh quy tắc 4 một chiều và route-as-source);
6. "thế hệ policy" (SetPolicy/SetUsers) ghi trong snapshot trước bằng thế hệ hiện tại (chống R4).

Mọi trường hợp khác (đổi tag/route/user, policy, user, autogroup:self, via, exit, deny-all chuyển trạng thái, ID trùng/view không hợp lệ) → **full rebuild như P00c**.

Trường hợp hẹp này đúng là ô allow_all (`*`) đang FAIL ở 10k và mọi join của node không nằm trong alias nào. Không giúp personal_devices (autogroup:self); đó là bước sau, khi M01 đã ổn.

Lưu ý phạm vi: với allow_all, mọi node đều "bị ảnh hưởng" bởi một join, nên **chỉ chọn người nhận không giảm fan-out**. Lợi ích thật đến từ việc gửi `NodeAdded(X)` (delta 1 peer) thay vì `PolicyChange` (PacketFilters + toàn bộ peer) khi filter không đổi. Đó là **đổi loại change**, có thể bị xem là đổi nội dung MapResponse, nằm ngoài "chỉ chọn người nhận" của ticket. **Cần chủ dự án quyết** (xem §7 Q1).

### 5.4 [Đề xuất] Kế hoạch nhỏ, RED trước

1. **Inventory + ADR addendum** (file này; ghi quyết định Q1–Q4 vào ADR 0005 hoặc ticket).
2. **Config switch, không hành vi**: `IncrementalNetmapConfig`, default, validate, `TestConfigFields`, `go generate`. RED: test default OFF và validate bounds. M05 harness bắt đầu ở đây (xem bước 3).
3. **M05 golden (RED/green ngay trên code P00c)**: harness `servertest` ghi chuỗi MapResponse đã chuẩn hóa (bỏ `ControlTime`, sort, redact key) cho kịch bản cố định (join, tag, route, xóa, đổi policy, user); commit golden; công tắc OFF phải khớp từng byte.
4. **API policy (chưa nối vào NodeStore)**: `SetNodes` trả chi tiết thay đổi (struct, giữ chữ ký `bool` cũ cho interface hoặc thêm method mới); `PolicyManager.PeersForNodes(...)` hoặc giữ lại `peerClass`/bitset của lần build trước. RED: rapid test "full build trên tập n+1 ≡ build cũ + phần tăng dần của X" trên `drawPeerMapNodes`/`drawPeerMapPolicy`, với gate báo `ISOLATION` trước như `TestBuildPeerMapIndexedMatchesPairwise`; nếu điều kiện §5.3 không thỏa thì API phải từ chối (trả "cần full").
5. **M01 NodeStore (RED)**: mở rộng `TestNodeStoreAdjacencyMatchesFullBuild`: thêm op join, xóa, đổi user, expiry, đổi policy (`SetPolicy`+`RebuildPeerMaps`), policy deny-all tenant (`WithEmptyPolicyDenies(true)`), exit node; chạy với công tắc ON; so **tập** peer (sort) sau **mỗi** bước bằng oracle PolicyManager mới tạo.
6. **M02 (RED)**: đổi policy / `SetUsers` / `rebuildPeerMaps` → counter `recomputed` tăng, `incremental` không tăng.
7. **Đường tăng dần trong `applyBatch`** khi đủ điều kiện §5.3; còn lại full. Cho M01, M02 xanh.
8. **Đối chiếu (M03, RED trước)**: op mới `reconcilePeerMaps` chạy **trên writer goroutine** (cùng snapshot và cùng trạng thái PolicyManager), dùng `pm.BuildPeerMap`, **không** gọi `policyPeersFunc` (nó gọi `SetNodes`, có thể kéo policy lùi, xem `TestStateRefreshDoesNotMovePolicyBack`). Lệch → metric, log redact (chỉ số lượng, không ID/IP), thay `peersByNode` bằng bản full, trả danh sách node lệch để `State` gửi full map **cho riêng các node đó** (`nc.collapsePendingToFull(nil)` qua API batcher mới, không dùng `AddWork(FullSelf)`, xem S1). Test tiêm lệch qua hook test.
9. **Chọn người nhận trong batcher (sau quyết định Q1)**: removal của node xóa gửi tới **hợp** của adjacency cũ **và** mọi node có X trong `lastSentPeers`/`partialPeers` (dựa trên những gì client đã nhận, không chỉ adjacency). Không bao giờ để removal phụ thuộc duy nhất vào adjacency tăng dần.
10. **M04 (RED)**: dùng harness `TestPendingBacklogCollapsesToCurrentState`/`TestAppendPendingBoundsFoldedPatches`: đầy hàng đợi rồi xóa node → node bị ảnh hưởng nhận removal hoặc full map không còn peer; full map định kỳ trải đều đúng chu kỳ (đồng hồ giả hoặc interval ngắn trong test).
11. **Race + benchmark** trên `mc-ci`/`mc-bench`, ma trận P00b/P00c, OFF và ON.
12. **M06** trên lab, rồi evidence và review bảo mật. Công tắc vẫn OFF mặc định.

## 6. Rủi ro (isolation: bỏ sót một trường hợp = BLOCK MERGE, AGENTS 13)

- **R1 — matcher đổi vì topology.** Join của node thuộc user/group/tag được nhắc trong policy đổi `pm.matchers`; vá adjacency chỉ cho X sẽ sai cho các cặp khác. Điều kiện 3 ở §5.3 chặn; cần test có policy `user@`, group, tag.
- **R2 — autogroup:self / via.** Thêm/bớt node đổi matcher riêng của mọi node cùng user (`invalidateAutogroupSelfCache`); bước đầu loại khỏi đường tăng dần.
- **R3 — quan hệ một chiều exit/capability** (`addCapSourceOnExit`): adjacency không đối xứng; thao tác "thêm X vào list của mọi peer của X" sai cho trường hợp này. Bước đầu loại node có route/exit/capSrc.
- **R4 — policy đổi giữa hai bước.** `State.SetPolicy` gọi `pm.SetPolicy` rồi mới `RebuildPeerMaps`; một batch tăng dần chạy giữa hai bước sẽ vá adjacency cũ bằng matcher mới và publish snapshot trộn. Cần ghi thế hệ policy trong snapshot (điều kiện 6) hoặc chặn tăng dần khi có rebuild đang chờ.
- **R5 — removal phụ thuộc adjacency cũ.** `DeleteNode` lấy target từ `prev.peersByNode`; nếu adjacency tăng dần thiếu R thì R giữ peer đã xóa. Hôm nay `PolicyChange` broadcast che; khi bỏ broadcast phải dựa vào `lastSentPeers` (bước 9).
- **R6 — `PeersChanged` không sinh removal.** Nếu đường mới thay `PolicyChange` bằng `NodeAdded` cho thay đổi làm **mất** visibility, client giữ peer cũ. Bước đầu chỉ cho join/xóa; mọi thay đổi khác vẫn `PolicyChange`.
- **R7 — PacketFilter/SSH/CapMap của node khác.** Join có thể đổi filter rút gọn hoặc SSH policy của node khác dù adjacency đúng (ví dụ SSH `autogroup:member`). Điều kiện 3 phải gồm SSH rules và nodeAttrs; nếu không, giữ `PolicyChange`.
- **R8 — Đối chiếu kéo policy lùi** nếu dùng `policyPeersFunc` ngoài writer (bước 8).
- **R9 — Chi phí đối chiếu/full map định kỳ** ở 10k có thể bằng đúng cái P00d muốn bớt; phải đo.
- **R10 — Metric cộng dồn mọi tenant**: không biết tenant nào lệch từ metric; cần log (đã redact) có tailnet ID của runtime, theo quy tắc log hiện có.

## 7. Giả định chưa kiểm chứng, câu hỏi mở, điều bất ngờ

**Giả định (chưa verify):**
- A1: Mọi thay đổi relation hôm nay đều sinh `PolicyChange` (suy ra từ `HasPolicyChange` dùng chung ở `updateChanges` và `nodesHavePolicyAffectingChanges`; chưa có test riêng khẳng định cho mọi call site trong `state.go`).
- A2: `TestNodeStoreAdjacencyMatchesFullBuild` chạy được với PolicyManager tạo bằng `WithEmptyPolicyDenies(true)` mà không cần đổi helper (helper hiện không truyền option).
- A3: Ô allow_all 10k sau P00c bị giới hạn bởi `PolicyChange` broadcast (map đầy đủ mỗi join), theo §7 của P00c; chưa có profile riêng xác nhận tỷ lệ encode vs build.
- A4: `servertest` đủ để ghi chuỗi MapResponse làm golden M05 (chưa thử; có thể cần hook ở batcher).
- A5: Client Tailscale bỏ peer khi nhận `PeersRemoved` delta và khi `Peers` của full map không còn peer đó (đã có trong comment `withRemovals`, chưa đo trên lab cho P00d).
- A6: `docs/ref/config-example.yaml` và `configuration-options.md` sinh bởi `tools/configgen/files` qua `go generate` (`config.go:4`); chưa chạy.
- A7: Không có ticker full map định kỳ nào khác ngoài các đường đã liệt kê (chỉ grep `FullUpdate()`/`FullSelf(` trong code không phải test).

**Câu hỏi cho chủ dự án:**
- Q1 (**đã chốt: có**, xem §5): Khi filter không đổi, gửi `NodeAdded(X)` thay `PolicyChange` cho join có được coi là "chọn người nhận" (trong phạm vi) không? Không có thay đổi này thì ô allow_all gần như không đổi.
- Q2 (**đã chốt**: như §5.1): Tên key và mặc định §5.1.
- Q3 (**đã chốt**: như §5.2): Bound M06 10 s và điều kiện "không nhờ full map/đối chiếu".
- Q4 (**đã chốt**: như §5.3): Phạm vi bước đầu §5.3 (join/xóa, chỉ policy global, không route/exit), autogroup:self để ticket sau.

**Điều bất ngờ:**
- S1: `change.IsFull()` không xét `TargetNode`, nên `AddWork(change.FullSelf(id))` sẽ khiến `addToBatch` ép full map cho **mọi** node của runtime, không chỉ `id`. Hôm nay `FullSelf` chỉ đi qua `MapResponseFromChange` (đồng bộ, initial map) nên không lộ. Full map định kỳ hoặc fallback của đối chiếu **không được** đi qua đường này.
- S2: Mỗi lần đăng ký node mới sinh `PolicyChange` broadcast (không phải `NodeAdded`) vì `SetNodes` báo đổi mỗi khi số node đổi, kể cả với policy `*`.
- S3: `setName` ép full peer recompute dù tên không phải input policy.
- S4: `allNodes` trong snapshot có thứ tự ngẫu nhiên (duyệt map), nên thứ tự trong `peersByNode` không ổn định giữa các snapshot; mapper sort peer theo ID nên MapResponse không bị ảnh hưởng. Test tương đương phải so **tập** (sort), như `checkAdjacencyMatchesFullBuild` đang làm.
- S5: "Xóa user" (M06) không trực tiếp xóa peer: `DestroyUser` từ chối khi user còn node. Kịch bản M06 phải xóa node trước rồi mới xóa user.

## 8. Tests + lệnh + exit code

Chưa chạy (inventory). Lệnh dự kiến theo ticket:
`go test ./hscontrol/state/... ./hscontrol/policy/... ./hscontrol/mapper/... -count=1`, `CGO_ENABLED=1 go test -race ...` trên `mc-ci`, `-rapid.checks` và seed ghi trong evidence.

## 9. E2E / negative isolation / migration / benchmark

- E2E M06: chưa chạy.
- Negative tenant isolation: chưa có code mới. P07 phải chạy lại M01/M03 với nhiều runtime.
- DB migration: không có (P00d không đổi schema).
- Benchmark: chưa có; before = P00c trên `master` (số đo P00c §6 vẫn là cây trước port).

## 10. Known limitations / security blockers

- Chưa có code; mọi mục §5 là đề xuất.
- Điều kiện §5.3 mục 3 cần API PolicyManager mới; chưa thiết kế chi tiết.
- Không bật công tắc ở bất kỳ môi trường nào trước review bảo mật.

## 11. Reviewer approval

Chưa có.
