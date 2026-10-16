**ONTHEGO 0.3.x 안정화 구현 계획 (전체 검토 보고서 개정판)**

작성일: 2026-10-16 (같은 날 개정)  
기준 커밋: `fcdc8d62278b32abc8fad2f013548a2a1e1a56bc`  
대상: ONTHEGO 0.3.0 소스, CLI, 테스트, 배포 구성 및 제품 사용 흐름  
문서 용도: **다음 구현의 착수 기준.** 항목마다 수정 방향을 결정해 두었다. 구현 중 결정을 바꿔야 하면 이 문서를 먼저 고친다.

**결론: 현재 구조는 유지한다. 이번 릴리스는 새 기능 없이 데이터 보존, 비밀 경계, 복구 경로만 닫는다.**

Git index와 working tree 분리, 제자리 적용, age 암호화와 signed manifest, 세션 바이트 보존, 인계 계보는 그대로 둔다. 이번 범위는 "성공했다고 보고했는데 내용이 사라지는 경우"와 "복구 자료가 있는데 복구할 수 없는 경우"를 없애는 것이다.

**0. 개정 요약**

원본 보고서의 재현 결과를 코드와 다시 대조했고, 구현 착수에 필요한 결정을 채웠다.

- R01–R18의 원인 설명은 모두 기준 커밋의 코드와 일치한다.
- **R19 신규 추가 (P1, 직접 재현):** 정책 변경으로 관리 대상에서 빠진 파일이 수신 측에서 경고 없이 삭제된다. R02와 R12를 원본 방향대로 고치면 이 경로가 바로 열린다. 그래서 R02와 R12보다 먼저 처리한다.
- 우선순위를 조정했다. R07, R08, R09는 P1에서 P2로 낮췄다. 세 항목 모두 수정 비용이 작아 일정상으로는 앞쪽에 둔다. 낮춘 이유는 각 항목에 적었다.
- 각 항목의 "검토한다", "설계한다", "정책을 정한다"를 구체적인 결정, 변경 위치, 테스트로 바꿨다.
- manifest, handoff 상태 파일, peer protocol의 호환성 규칙을 2절에 추가했다. 여러 항목이 같은 schema 변경을 공유하므로 한 번에 올린다.
- S, U 항목은 이번 범위에 꼭 필요한 일부만 남겼다. 나머지는 사유와 함께 보류 목록(9절)으로 옮겼다.
- 측정 수치, coverage, 유지할 설계 설명은 근거 부록(10절)으로 줄였다.

**1. 이번 릴리스 범위와 순서**

| 단계 | 항목 | 목표 | 규모 |
| --- | --- | --- | --- |
| 1. 손실과 비밀 | R19, R01, R02, R04(+S02 일부), R03 | 성공 보고 뒤 내용 손실 0건. 자동 선택 비밀의 Git 객체 저장 0건 | L |
| 2. 복구 | R06, R08, R07, R05 | 보관 중인 package나 recovery 자료만으로 복구 가능 | M |
| 3. 취소 | R09, R10 | native와 npm 경로 모두 정해진 시간 안에 종료, orphan 없음 | S |
| 4. 선택과 진단 | R11, R12, R13, R14, R15, R16, R17, R18 | preview와 실제 실행이 같은 검증을 공유. 누락과 부분 성공이 출력에 드러남 | M |
| 5. 경량 견고화 | DEP, S01 일부, S10 | 알려진 의존성 취약점 갱신, 파일 단위 원자적 교체, 터미널 출력 escape | S |

규모 표기: S는 반나절 이하, M은 1–2일, L은 3일 이상.

단계 1과 2는 순서를 지킨다. 단계 3–5는 단계 1의 schema 변경이 끝난 뒤라면 병렬로 진행해도 된다. 항목별로 커밋을 나누고, 각 커밋은 회귀 테스트를 먼저 추가해 실패를 확인한 뒤 수정을 넣는다.

**2. 공통 구현 규칙**

**호환성**

- **Snapshot manifest:** `verifyExtractedContext`는 `DisallowUnknownFields`를 쓴다. 따라서 필드를 추가하면 구버전 수신기가 package를 거부한다. R19와 R04가 추가하는 필드는 한 번에 넣고 `snapshot.SchemaVersion`을 2에서 3으로 올린다. 새 수신기는 2와 3을 모두 받는다. 구버전 수신기는 3을 `manifest invalid`로 거부하며, 이 점을 release note에 적는다.
- **Peer protocol:** `negotiatePeer`는 로컬의 모든 capability를 peer에게 요구한다. wire 동작이 바뀌면 capability를 추가한다. 이번에 추가할 것은 `snapshot-v3`과 `session-scope`(R11)이다. 양쪽을 함께 업데이트해야 한다는 기존 정책을 유지한다.
- **Handoff 상태 (`current.json`, pending, peer receipt):** `readJSON`이 `DisallowUnknownFields`이고 `validateBaseline`이 `SchemaVersion == 2`만 허용한다. 필드를 추가하면 downgrade한 바이너리가 상태를 읽지 못한다. 그래서 새 상태는 **별도 파일**로 둔다(R06의 `session-recovery.json`). 예외는 R11의 `Selection.None`이며, `omitempty`로 사용할 때만 기록한다.
- **프로젝트 정책 (`.onthego.yaml`):** schema 2에만 알 수 없는 키 거부를 적용한다(R03). schema 1의 과거 cloud 설정은 지금처럼 버린다.

**오류**

- 새로 만드는 실패는 모두 `otgerror` typed error로 만든다. 사용자 입력 오류는 `E_INPUT`(exit 2)이다.
- untyped error는 R18에서 `E_INTERNAL`(exit 1)로 분리한다. 이후 `E_INPUT`은 실제 입력 오류에만 쓴다.

**테스트**

- 기존 helper(`paired`, `newMachine`, `newProject`, `codexSession`, `assertSecretNotStoredInGit`, `assertNoPlaintextStaging`)를 재사용한다.
- 파일시스템 특성에 의존하는 테스트는 대상 디렉터리에서 probe한 뒤 해당하지 않으면 `t.Skip`한다. OS 이름으로 분기하지 않는다.
- 크기 경계 테스트를 위해 `packageotg.MaxPayloadSize` 같은 상수를 테스트에서 낮출 수 있는 package 변수로 바꾸는 것은 허용한다.

**되돌리지 않을 것**

- 디렉터리 통째 교체 방식으로 돌아가지 않는다.
- 이름에 `token`, `secret`이 들어간 모든 파일을 막던 넓은 규칙으로 돌아가지 않는다.
- 세션 실패가 workspace 적용을 막는 구조로 돌아가지 않는다.

**3. 단계 1: 손실과 비밀 경계**

**R19. P1 (신규): 관리 해제된 파일이 수신 측에서 삭제된다**

근거: [restore.go:428](/Users/johnnybae/workspace/onthego/internal/restore/restore.go:428) `managedBefore`, [restore.go:512](/Users/johnnybae/workspace/onthego/internal/restore/restore.go:512) `applyState`의 삭제 루프, [sync.go:440](/Users/johnnybae/workspace/onthego/internal/syncer/sync.go:440) `previous.Managed` 전달.

재현: 미추적 `notes.txt`를 첫 handoff로 보냈다. 원본에서 `.onthego.yaml`의 `exclude`에 `notes.txt`를 추가하고, 파일은 원본에 그대로 둔 채 다시 export하고 apply했다. apply는 성공했지만 수신 측 `notes.txt`가 삭제됐다. backup ref는 생겼고 warning은 비어 있었다. 사용자는 삭제 사실을 알 방법이 없다.

원인: 수신기는 이전 handoff의 관리 목록 중 새 desired에 없는 경로를 지운다. 이때 "원본에서 삭제됨"과 "원본에는 있지만 더 이상 보내지 않음"을 구분할 정보가 package에 없다.

영향: R02를 고쳐 `.npmrc`를 자동 선택에서 빼면, 다음 handoff에서 기존 수신 장치의 `.npmrc`가 모두 지워진다. R12에서 persisted secret을 exclude로 해제할 때도 같은 일이 생긴다.

결정:

1. manifest(schema 3)에 서명된 `retained_paths []string`을 추가한다. 송신 측은 `previous.Managed`에서 이번 선택을 뺀 경로 중 **원본에 아직 존재하는 경로**만 넣는다. 원본에서 실제로 지운 파일은 넣지 않으므로 삭제는 지금처럼 전파된다. ACK가 없는 offline 왕복에서는 가장 최근의 유효한 pending/baseline을 먼저 선택하고 fingerprint 캐시에 Managed도 포함하여 이전 관리 목록이 되살아나지 않게 한다.
2. 수신 측 `applyState`는 `retained_paths`를 삭제 후보에서 제외하고 디스크에 그대로 둔다. 새 baseline의 `Managed`에서는 뺀다. 결과에 `Warnings: "No longer managed; left in place: <path>"`를 추가한다.
3. 검증 조건: `retained_paths`는 `ProjectPath`를 통과하고, 중복이 없고, desired와 겹치지 않고, `MaxFiles` 이하여야 한다. 위반하면 `E_PACKAGE_INVALID`로 거부한다.
4. Git이 추적하는 파일(index, HEAD에 있는 경로)의 삭제 의미는 바꾸지 않는다. Git과 같은 동작이기 때문이다.
5. 나중에 원본이 같은 경로를 다시 포함하면, 수신 측에 남은 사본은 unmanaged 파일이므로 기존 `checkCollisions`가 내용 비교 후 거부한다. 오류 메시지에 "이전에 관리 해제되어 남은 파일"일 수 있다는 안내를 추가한다.

테스트: 위 재현을 회귀 테스트로 만든다. 원본에서 실제로 지운 미추적 파일은 수신 측에서도 지워지는지 확인한다. secret 파일을 exclude로 해제하는 경우는 R12 테스트와 함께 확인한다.

완료 기준: 정책만 바뀐 경로는 수신 측에서 바이트 그대로 남고 출력에 안내된다. 원본에서 삭제한 경로의 삭제는 계속 전파된다.

**R01. P1: 대소문자와 Unicode 정규화 충돌로 성공한 복원에서 내용이 사라진다**

근거: [restore.go:407](/Users/johnnybae/workspace/onthego/internal/restore/restore.go:407) `desiredEntries`, [restore.go:457](/Users/johnnybae/workspace/onthego/internal/restore/restore.go:457) `checkCollisions`, [restore.go:597](/Users/johnnybae/workspace/onthego/internal/restore/restore.go:597) `applyState`의 쓰기.

재현(원본): Linux에서 성립하는 `Foo.txt`(`UPPER`)와 `foo.txt`(`lower`)가 함께 있는 tree를 plumbing으로 만들고 정상 서명했다. 이를 macOS의 대소문자 비구분 대상에 적용했다. `apply_error=<nil>`이었고, 두 경로가 같은 파일이 되어 내용이 `lower`만 남았다. 성공 receipt까지 생성됐다.

원인: 중복과 parent 검사를 문자열 기준으로만 한다. 대상 파일시스템이 두 이름을 같은 파일로 해석하는지 확인하지 않는다.

결정:

1. `restore`에 `pathKeyer(root)`를 추가한다. 대상 root 안에서 임시 probe 파일 두 개로 확인한다.
   - 대소문자 비구분 여부: 소문자 이름으로 만든 파일을 대문자 이름으로 `Lstat`
   - 정규화 비구분 여부: NFC 이름으로 만든 파일을 NFD 이름으로 `Lstat`
   - 결과에 따라 key 함수는 원문, NFD, case fold+NFD 중 하나가 된다.
   - 의존성으로 `golang.org/x/text`(`cases`, `unicode/norm`)를 추가한다. 공식 확장 모듈이므로 허용한다.
2. `checkCollisions` 첫 단계에서 desired 전체(worktree tree, untracked tree, secret entry)의 key 중복을 검사한다. 중복이 있으면 **어떤 파일도 쓰기 전에** `E_RESTORE_FAILED`로 실패한다. 메시지는 "paths collide on this filesystem"이고 충돌 쌍을 최대 10개까지 나열한다. 같은 함수를 쓰는 `Undo`도 자동으로 보호된다.
3. 다층 방어로, `applyState`가 이번 실행에서 쓴 경로의 `FileInfo`를 모은다. 서로 `os.SameFile`인 쌍이 나오면 실패로 끝내고 성공 baseline을 저장하지 않는다. 오류에는 backup ref를 담아 `undo`를 안내한다.
4. 사후에 모든 파일을 재해시하는 검증은 하지 않는다. 비용이 크고, 2와 3으로 이 결함의 경로가 닫힌다.
5. 대상 저장소의 HEAD 자체가 충돌 경로를 포함하면 handoff는 계속 거부된다. 이는 의도한 동작이다. macOS에서는 Git checkout도 이미 깨지는 상태이므로, 오류 메시지에서 Git에서 이름을 바꾸라고 안내한다.

테스트: key 함수 단위 테스트. plumbing으로 만든 충돌 tree에 대해 대소문자 쌍과 NFC/NFD 쌍 각각에서 적용 전 거부와 대상 불변을 확인한다(대소문자 구분 FS에서는 skip). 대소문자만 바꾼 rename(`Foo.txt`에서 `foo.txt`)이 정상 적용되는지도 확인한다.

완료 기준: 충돌 fixture는 대상의 어떤 파일도 바꾸기 전에 실패한다. 성공한 적용에서 서로 다른 desired 경로가 같은 inode를 가리키는 일이 없다.

**R02. P1: 자격 증명 파일이 자동 선택되어 평문 Git 객체로 남는다**

근거: [capture.go:258](/Users/johnnybae/workspace/onthego/internal/capture/capture.go:258) `selectFiles`, [capture.go:624](/Users/johnnybae/workspace/onthego/internal/capture/capture.go:624) `isSecretCandidate`, [wip.go:103](/Users/johnnybae/workspace/onthego/internal/gitx/wip.go:103) untracked tree 생성.

재현(원본): 미추적 `.npmrc`에 합성 `_authToken`을 넣었다. inspect는 `secret=false, policy=untracked`로 표시했고, push 후 원본 저장소에서 해당 blob이 `git cat-file -e`로 조회됐다.

추가 확인: 이 blob은 일회성이 아니다. WIP commit은 이전 WIP를 parent로 잇고(`CaptureWIP`의 네 번째 parent), `refs/onthego/workspaces/*`가 최신 WIP를 가리킨다. 따라서 한 번 들어간 blob은 이후 GC로도 회수되지 않는다. bundle로 전송되므로 수신 측 객체 저장소에도 같은 방식으로 남는다.

결정:

1. 이름 기반 secret candidate를 확장한다. 내용은 읽지 않는다.
   - 정확한 파일명: `.npmrc`, `.yarnrc`, `.yarnrc.yml`, `.pypirc`, `.netrc`, `_netrc`, `.git-credentials`, `.pgpass`, `.envrc`, `.dockercfg`, `kubeconfig`
   - 접미사: `.pfx`, `.jks`, `.keystore`, `.tfvars`, `.tfstate`, `.tfstate.backup`
   - `.key`는 넣지 않는다. Keynote 파일 오탐 문제가 이전 리뷰에서 지적됐다.
2. 내용 검사(`_authToken` 탐지 등)는 하지 않는다. 결과가 결정적이어야 하고, 선택 단계에서 파일 내용을 읽지 않는 원칙을 유지하기 위해서다. 인증 정보가 없는 `.npmrc`를 보내려면 `--include-secret`(암호화된 별도 payload)을 쓰거나 Git에 커밋한다.
3. secret candidate는 지금처럼 자동 선택에서 빠지고 preview, inspect, push 경고에 "secret candidate" 사유로 나타난다.
4. 기존 수신 장치의 `.npmrc`는 R19의 `retained_paths`로 보존된다. 구버전 baseline의 변경 감지에는 이전 Managed 경로를 계속 반영하되, 새 export 선택과 fingerprint에서는 제거하여 업그레이드 직후의 거짓 충돌을 막는다. **R19 없이 R02만 배포하면 안 된다.**
5. 이미 저장된 객체는 이번에 지우지 않는다. 저장소 정리 도구는 S04와 함께 보류한다. 대신 release note에 다음을 적는다. 0.3.0 이하에서 미추적 `.npmrc`류 파일을 handoff한 사용자는 해당 토큰을 교체해야 한다. 그 바이트는 원본과 수신 측의 `.git/objects`에 남아 있다.

테스트: 합성 토큰이 든 미추적 `.npmrc`는 선택되지 않고 preview에 사유가 나온다. `--include-secret .npmrc`로 보낸 경우 `assertSecretNotStoredInGit`가 원본과 수신 측 모두에서 통과한다. `src/tokenizer.ts`와 `docs/secret-handling.md`는 계속 자동 선택된다.

완료 기준: 확장 목록의 파일은 자동 선택되지 않는다. 명시적으로 허용한 경우에도 원본과 수신 측의 Git 객체 저장소에 해당 blob이 없다.

**R04. P1: 생성기가 만든 고압축률 package를 수신기가 거부한다**

근거: [package.go:190](/Users/johnnybae/workspace/onthego/internal/packageotg/package.go:190) 수신 측 `MaxRatio` 검사, [package.go:385](/Users/johnnybae/workspace/onthego/internal/packageotg/package.go:385) 생성 측 `checkPayloadLimitsContext`.

재현(원본): 반복 본문 약 2MiB의 합성 Codex JSONL이 3,484바이트 `.otg`로 생성됐다. 같은 장치에서 restore하자 `E_PACKAGE_INVALID: archive extraction limit exceeded`(exit 5)가 났다.

판단: 압축률 규칙은 실제로 막는 것이 없다. bundle이 아닌 entry의 총량은 이미 절대 상한 `MaxPayloadSize`(250MiB)로 양쪽에서 같은 방식으로 제한된다. `repo.bundle`은 압축률 검사에서 빠져 있으므로, 압축률로 증폭을 막는다는 목적도 성립하지 않는다. 남는 효과는 정상 package를 복원하지 못하게 하는 것뿐이다.

결정:

1. `ExtractContext`에서 `MaxRatio` 검사를 제거한다. 절대 상한과 zstd decoder 메모리 상한은 유지한다.
2. 대신 미인증 입력의 자원 소모는 **서명 우선 검증**으로 막는다(S02 일부).
   - `CreateContext`는 이미 `manifest.json`, `manifest.sig`를 tar의 첫 두 entry로 쓴다. 수신기는 이 순서를 필수로 요구한다.
   - 두 entry를 각각 8MiB 이하로 메모리에 읽은 뒤 signer, 서명, schema를 먼저 검증한다. 그 다음 entry부터 디스크에 쓴다.
   - 이후 entry 이름이 signed `PayloadHashes`에 없으면 쓰기 전에 거부한다.
   - schema 3에는 signed `payload_sizes map[string]int64`를 추가한다. 각 entry의 `header.Size`가 서명된 크기와 정확히 일치해야 한다. 이로써 `repo.bundle`도 서명된 크기로 제한된다.
   - schema 2 package에는 서명 우선 검증과 이름 검사만 적용하고, 크기는 기존 절대 상한으로 제한한다. 과거 생성기가 넣은 안전한 경로의 크기 0인 directory entry는 디스크에 만들지 않고 건너뛴다. 실제로 쓰는 파일은 모두 서명된 이름이어야 한다.
3. 생성과 수신의 크기 계산(manifest와 서명 포함, bundle 제외)이 같다는 것을 테스트로 고정한다.

테스트: 고압축률 세션과 고압축률 supplemental secret의 왕복. 낮춘 `MaxPayloadSize`로 경계값 바로 아래는 왕복하고 바로 위는 pack 단계에서 거부되는지 확인한다. 서명이 틀린 package는 payload를 하나도 쓰기 전에 거부되는지 확인한다(임시 디렉터리에 manifest 외 파일이 없음). 서명된 크기와 실제 크기가 다른 entry는 거부되는지 확인한다.

완료 기준: `Create`가 성공한 package는 같은 버전에서 항상 복원된다. 서명되지 않은 package는 manifest 크기 이상의 디스크를 쓰지 않는다.

**R03. P2: 잘못된 agent 값이 전송 범위를 넓힌다**

근거: [config.go:67](/Users/johnnybae/workspace/onthego/internal/config/config.go:67) `Load`, [sync.go:653](/Users/johnnybae/workspace/onthego/internal/syncer/sync.go:653) `sessionRoots`, [inspect.go:26](/Users/johnnybae/workspace/onthego/internal/cli/inspect.go:26), [restore.go:79](/Users/johnnybae/workspace/onthego/internal/restore/restore.go:79) incoming `Config`.

재현(원본): `agent: typo`에서 inspect가 exit 0이었고 Codex 세션이 선택됐다.

결정:

1. `config.Validate(cfg)`를 만든다. agent는 `auto|codex|claude|none` 중 하나여야 한다. project ID와 schema 검사도 여기로 모은다.
2. 이 함수를 `Load`, `Save`, `restore.Config`(incoming 정책) 세 곳에서 모두 호출한다. incoming 정책이 유효하지 않으면 workspace를 바꾸기 전에 `E_PACKAGE_INVALID`로 거부한다.
3. schema 2 파일은 `yaml.v3` Decoder의 `KnownFields(true)`로 읽어 알 수 없는 키를 거부한다. schema 1은 지금처럼 관대하게 읽는다.
4. 로컬 오류는 `E_INPUT`이다. 메시지에 파일 경로, 잘못된 값, 허용값을 담는다.
5. `inspect.go`의 agent switch 중복을 없애고 `syncer.SessionRoots(cfg)`를 공개해 공유한다(S12의 일부).

완료 기준: 잘못된 값이나 알 수 없는 키는 세션 검색 전에 실패한다. inspect, push, pull, snapshot이 같은 root 선택 함수를 쓴다.

**4. 단계 2: 복구 경로**

**R06. P1: 세션 import 실패 후 같은 offline package로 재시도해도 복구되지 않는다**

근거: [sync.go:386](/Users/johnnybae/workspace/onthego/internal/syncer/sync.go:386) 동일 snapshot 조기 반환, [sync.go:446](/Users/johnnybae/workspace/onthego/internal/syncer/sync.go:446) import 실패 처리, [sync.go:480](/Users/johnnybae/workspace/onthego/internal/syncer/sync.go:480) baseline 저장.

재현(원본): import를 실패시킨 뒤 원인을 제거하고 같은 package를 restore했다. 결과는 `already_applied=true`, imported 0이었다.

전제 확인: `planImport`는 동일 내용을 skip하고 append 확장만 갱신한다. 따라서 세션 import는 재실행해도 안전하다(멱등). 일부 adapter만 성공했던 경우에도 다시 실행할 수 있다.

결정:

1. import가 실패하면 기존 `session-recovery-*` 디렉터리를 보존한다. 추가로 `handoff/<root digest>/session-recovery.json`에 `pending` 배열로 각 실패의 `snapshot_id`, `recovery_dir`, `created_at`, 공개 가능한 오류 요약을 기록한다. 연속된 handoff 실패도 이전 기록을 덮지 않는다. `sessions recover`는 오래된 실패부터 모두 시도하고 실패한 기록만 남긴다. `current.json` schema는 바꾸지 않는다.
2. `ApplyWithOptions`의 동일 snapshot 분기에서 `session-recovery.json`의 snapshot이 같으면 workspace 단계를 건너뛰고, 방금 추출한 `native-sessions`로 세션 import만 다시 실행한다.
   - 성공하면 기록과 recovery 디렉터리를 지우고, baseline의 `SessionFingerprint`만 갱신한다.
   - 결과는 `already_applied=true`, `sessions_retried=true`이다.
   - workspace 파일은 건드리지 않으므로 그 사이의 편집은 그대로 보존된다.
3. 새 명령 `onthego sessions recover [--session-conflict ...]`를 추가한다. package 없이 recovery 디렉터리로 2와 같은 작업을 한다. project root는 `FindRoot`로 찾는다. recovery 자료는 추출 시 인증됐고 0700 상태 디렉터리에 있다. `planImport`가 세션 manifest의 hash를 다시 검증한다.
4. `status`는 미완료 recovery가 있으면 warning과 JSON 필드 `session_recovery_pending`를 출력한다.
5. 종료 코드는 지금처럼 0을 유지한다. peer apply의 성공 의미와 lost-ack 복구 흐름을 바꾸지 않기 위해서다. 대신 결과 JSON에 `sessions.status: "recovery_needed"`를 추가하고, text 출력 첫 줄에 표시한다.

테스트: 첫 import 실패, 대상 파일 추가 편집, 환경 복구, 같은 package 재시도 순서로 실행한다. 편집이 유지되고 세션만 들어오는지 확인한다. `sessions recover` 경로도 같은 시나리오로 확인한다. Codex만 성공한 부분 실패 뒤의 재시도도 확인한다.

완료 기준: 원본 장치 없이 보관한 package나 recovery 자료만으로 세션을 복구할 수 있다.

**R08. P2 (원본 P1): 정책 파일이 없으면 undo에 접근할 수 없다**

근거: [cli/backups.go:49](/Users/johnnybae/workspace/onthego/internal/cli/backups.go:49) `loadProject`, [root.go:78](/Users/johnnybae/workspace/onthego/internal/cli/root.go:78), [sync.go:599](/Users/johnnybae/workspace/onthego/internal/syncer/sync.go:599) `Undo`의 `config.Load`.

등급 조정 이유: `.onthego.yaml`은 보통 커밋되어 있어 `git checkout`으로 복구할 수 있고, backup 자체는 손상되지 않는다. 다만 현재 안내대로 `onthego init`을 실행하면 새 project ID가 생겨 상태가 꼬인다. 수정 비용이 작으므로 이번 단계에서 고친다.

결정:

1. `undo`, `backups list`, `backups prune`는 `FindRoot`와 `gitx.Validate`만으로 시작한다. `restore.Undo`와 `restore.Backups`는 원래 정책 파일이 필요 없다.
2. `syncer.Undo`는 `config.Load` 실패를 허용한다. baseline 삭제는 project ID 대신 `projects/*/handoff/<root digest>/current.json`을 glob으로 찾아 처리한다.
3. `loadProject`의 "run onthego init" 오류는 이 worktree에 ONTHEGO backup이 있을 때 문구를 바꾼다. `onthego undo` 또는 `git checkout -- .onthego.yaml`을 먼저 안내하고, init은 새 project ID를 만든다고 경고한다.

완료 기준: 정책 파일 삭제, YAML 손상, 잘못된 project ID 각각에서 `backups list`와 `undo <ref>`가 동작한다.

**R07. P2 (원본 P1): backups prune이 workspace lock을 무시한다**

근거: [cli/backups.go:33](/Users/johnnybae/workspace/onthego/internal/cli/backups.go:33), [restore/backups.go:222](/Users/johnnybae/workspace/onthego/internal/restore/backups.go:222), [state.go:65](/Users/johnnybae/workspace/onthego/internal/syncer/state.go:65).

등급 조정 이유: 사용자가 apply나 undo와 동시에 prune을 직접 실행해야만 발생한다. 실제 손실까지 재현되지는 않았다. 경로가 존재하고 수정이 한 함수 수준이라 이번에 고친다.

결정:

1. `syncer.PruneBackups(ctx, root, keep)`를 만든다. `lock(root)`를 잡은 뒤 `restore.PruneBackups`를 호출한다. CLI는 이 함수만 쓴다.
2. 이미 lock을 보유한 `Apply`와 `Undo` 내부 정리는 지금처럼 `restore.PruneBackups`를 직접 호출한다. `fsx.Lock`이 non-blocking이라 lock 중 prune은 자연스럽게 `E_PRECONDITION`으로 실패한다.
3. CLI `--keep` 기본값 10을 `restore.DefaultBackupLimit`(5)으로 맞춘다. 매 apply가 5개로 줄이므로 10은 의미가 없다.

완료 기준: lock 보유 중 prune은 ref 변경 없이 `E_PRECONDITION`(exit 3)으로 끝난다.

**R05. P2: pending receipt의 WIP 객체가 GC로 사라져 정상 귀환이 실패한다**

근거: [wip.go:180](/Users/johnnybae/workspace/onthego/internal/gitx/wip.go:180) workspace ref 교체, [sync.go:513](/Users/johnnybae/workspace/onthego/internal/syncer/sync.go:513) `acceptedBaseline`, [state.go:154](/Users/johnnybae/workspace/onthego/internal/syncer/state.go:154) `pendingBaselines`.

재현(원본): 미확인 export 3개를 만든 뒤 `git gc --prune=now`를 실행했다. 이어서 첫 export의 귀환 package를 적용하자 `merge-base ... Not a valid commit name`으로 실패했다.

결정:

1. export가 pending을 기록할 때 `refs/onthego/pending/<snapshotID>`가 WIP commit을 가리키게 한다.
2. 이 ref는 다음 시점에 지운다. acknowledge로 pending JSON을 지울 때, `pendingBaselines`의 TTL(30일) 만료나 64개 초과로 정리할 때, 대응하는 JSON이 없는 고아 ref를 발견했을 때.
3. `acceptedBaseline`은 후보 WIP가 로컬에 없으면(`cat-file -e` 실패) 그 후보만 건너뛰고 다음 후보를 계속 본다. 그 밖의 Git 오류만 중단한다. 이전 버전이 남긴 ref 없는 pending도 같은 방식으로 처리된다.
4. `gitx.Bundle`은 `refs/heads|tags|remotes|stash`만 포함하므로 pending ref는 전송되지 않는다. 이 점을 테스트로 고정한다.

완료 기준: 여러 미확인 export, 강제 GC, 이전 export의 귀환 순서에서 `--replace` 없이 수신된다.

**5. 단계 3: 취소와 프로세스 생명주기**

**R09. P2 (원본 P1): stdin을 기다리는 peer apply가 SIGTERM에 종료되지 않는다**

근거: [peer.go:87](/Users/johnnybae/workspace/onthego/internal/cli/peer.go:87), [main.go:38](/Users/johnnybae/workspace/onthego/cmd/onthego/main.go:38).

등급 조정 이유: stdin 복사 중에는 workspace lock을 잡지 않고 아무것도 수정하지 않는다. 실제 SSH 경로에서는 연결이 끊기면 sshd가 stdin을 닫아 `io.Copy`가 끝난다. 영향은 남는 프로세스와 임시 파일이며, 데이터 손상은 없다.

결정:

1. `peer apply`의 복사를 goroutine에서 실행하고 `ctx.Done()`과 select한다. 취소되면 바로 context 오류를 반환한다. main이 exit 130으로 끝나며 defer로 임시 package가 정리된다. 막힌 read는 프로세스 종료와 함께 사라지므로 fd를 닫는 소유권 설계는 필요 없다.
2. 입력 크기에 상한을 둔다: `MaxBundleSize + MaxPayloadSize + 16MiB`. `io.LimitReader`로 상한을 넘으면 `E_PACKAGE_INVALID`이다(S02 일부).
3. `runCommand`에서 첫 신호로 ctx가 취소되면 `stop()`을 호출해 기본 signal 동작을 복원한다. 두 번째 Ctrl-C는 즉시 종료된다.

완료 기준: 무입력과 부분 입력 상태에서 SIGINT와 SIGTERM을 받으면 1초 안에 종료되고 임시 디렉터리가 남지 않는다.

**R10. P2: npm launcher만 SIGTERM을 받으면 native child가 남는다**

근거: [onthego.js:27](/Users/johnnybae/workspace/onthego/npm/bin/onthego.js:27) `spawnSync`.

결정: 비동기 `spawn`(stdio inherit)으로 바꾼다. Node 18 이상을 기준으로 한다.

- `SIGINT`, `SIGTERM`, `SIGHUP`을 child에 전달한다.
- child가 signal로 끝나면 handler를 제거하고 `process.kill(process.pid, signal)`로 같은 signal로 종료한다. 정상 종료면 같은 exit code로 끝낸다.
- spawn 오류는 exit 1로 끝낸다.

테스트: `scripts/sigint-e2e.py`와 같은 방식의 스크립트로 launcher PID에만 SIGTERM을 보낸 경우와 process group 전체에 보낸 경우를 확인한다. 둘 다 child가 남지 않고 exit code가 native 실행과 같아야 한다.

**6. 단계 4: 선택과 진단 계약**

**R11. P2: `--sessions skip`이 세션 제외처럼 보인다**

근거: [transfer.go:150](/Users/johnnybae/workspace/onthego/internal/cli/transfer.go:150), [sessions.go:280](/Users/johnnybae/workspace/onthego/internal/agentsessions/sessions.go:280), [README.md:94](/Users/johnnybae/workspace/onthego/README.md:94).

결정:

1. `push`, `pull`, `snapshot pack`에 `--no-sessions`를 추가한다. `Selection.None=true`이면 송신 측이 세션 검색을 하지 않고, package에 `native-sessions`가 없으며, 수신 측 agent home도 건드리지 않는다.
2. pull은 원격 export에 `--no-sessions`를 넘긴다. 이를 위해 capability `session-scope`를 추가한다.
3. 충돌 정책 플래그의 사용자용 이름은 `--session-conflict skip|theirs|keep-both`로 바꾼다. `--sessions`는 숨긴 deprecated alias로 남기고 사용 시 경고한다. 내부 `peer apply --sessions`는 호환을 위해 그대로 둔다.
4. README 94행 예시와 99행 설명을 고친다.

완료 기준: `--no-sessions` 인계에서 세션 검색, payload 포함, 대상 agent home 변경이 모두 없다.

**R12. P2: 이전에 포함한 secret을 exclude로 해제하면 인계가 막힌다**

근거: [sync.go:647](/Users/johnnybae/workspace/onthego/internal/syncer/sync.go:647) `effectiveOptions`, [capture.go:303](/Users/johnnybae/workspace/onthego/internal/capture/capture.go:303).

결정:

1. 우선순위를 "프로젝트 exclude가 baseline에 저장된 include-secret보다 앞선다"로 정한다.
2. 저장된 secret이 exclude와 겹치면 오류 대신 effective 목록에서 빼고 warning을 출력한다. 이 경로는 R19의 `retained_paths`로 전달되어 수신 측 사본이 그대로 남는다.
3. 명령줄에서 직접 준 `--include-secret X`가 exclude와 겹치면 지금처럼 `E_SECRET_POLICY`로 실패한다. 이는 사용자의 명시적 요청이 서로 충돌하는 경우다.
4. 별도 `secrets forget` 명령은 보류한다(U07). exclude로 같은 의도를 표현할 수 있다.

완료 기준: exclude 추가 후 push가 성공한다. 원본의 비밀은 유지되고, 이후 payload에서 빠지며, 수신 측 사본은 남고 출력에 안내된다.

**R13. P2: dry-run이 실제 실행의 로컬 검증을 대표하지 못한다**

근거: [transfer.go:77](/Users/johnnybae/workspace/onthego/internal/cli/transfer.go:77), [snapshot.go:24](/Users/johnnybae/workspace/onthego/internal/cli/snapshot.go:24).

결정:

1. `validateTransferOptions`를 만들어 dry-run 분기보다 먼저 호출한다. 검사 대상은 세션 선택값(`session-days >= 0`, 빈 ID 금지, `--session`과 `--all-sessions` 동시 사용 금지), 충돌 정책, include 경로다. 실패는 `E_INPUT`이다.
2. dry-run은 로컬 세션 검색 결과와 warning도 함께 보여준다.
3. dry-run JSON에 `not_checked: ["peer", "destination_conflict"]`를 넣어 생략한 검증을 명시한다.
4. tracked 변경 diff 표시(U01)는 보류한다.

완료 기준: 로컬 인수 오류는 dry-run과 실제 실행에서 같은 코드로 실패한다.

**R14. P2: init이 ignore한 `.env`가 제외 안내에 나타나지 않는다**

근거: [config.go:18](/Users/johnnybae/workspace/onthego/internal/config/config.go:18), [capture.go:238](/Users/johnnybae/workspace/onthego/internal/capture/capture.go:238) `excludedFiles`.

결정: `excludedFiles`에서 `git ls-files --others --ignored --exclude-standard --directory -z`를 추가로 실행한다. 반환된 항목 중 basename이 secret candidate나 always-denied인 것만 "ignored secret candidate; not transferred"로 보고한다. `--directory`가 ignore된 디렉터리(`node_modules/` 등)를 한 항목으로 접으므로 출력 크기가 제한된다.

완료 기준: init 직후 만든 `.env`가 inspect와 dry-run에 사유와 함께 나타난다.

**R15. P2: 없는 세션을 명시해도 offline pack이 성공으로 보인다**

근거: [discovery.go:278](/Users/johnnybae/workspace/onthego/internal/agentsessions/discovery.go:278), [sync.go:130](/Users/johnnybae/workspace/onthego/internal/syncer/sync.go:130), [snapshot.go:46](/Users/johnnybae/workspace/onthego/internal/cli/snapshot.go:46).

결정:

1. `--session`으로 명시한 ID가 하나라도 없으면 package를 만들기 전에 `E_INPUT`으로 실패하고 누락 ID를 나열한다. pack, push, peer export 모두 같다. 자동 선택에서 malformed 세션을 건너뛰는 경우는 지금처럼 warning으로 둔다.
2. `ExportWithOptions`가 manifest와 함께 세션 warning을 반환하도록 결과 타입을 바꾼다. pack은 JSON `warnings`로, push는 `result.Warnings`로 출력한다.

**R16. P2: inspect가 손상 세션 경고를 버린다**

근거: [inspect.go:34](/Users/johnnybae/workspace/onthego/internal/cli/inspect.go:34), [discovery.go:95](/Users/johnnybae/workspace/onthego/internal/agentsessions/discovery.go:95).

결정: inspect는 `DiscoverWithOptions`와 R03의 `SessionRoots`를 사용하고, warning을 text와 JSON(`session_warnings`)으로 출력한다. warning 문구에는 경로와 세션 ID만 넣고 내용은 넣지 않는다(현재 형식 유지).

**R17. P2: 쓰지 않는 adapter의 home 오류가 필요한 세션 처리까지 막는다**

근거: [sessions.go:324](/Users/johnnybae/workspace/onthego/internal/agentsessions/sessions.go:324) `ImportWithOptions`, [discovery.go:138](/Users/johnnybae/workspace/onthego/internal/agentsessions/discovery.go:138).

결정:

1. 수신: `ImportWithOptions`는 세션 manifest를 먼저 읽어 실제로 포함된 agent를 구한다. 그 agent의 home만 검증하고, 만들고, 잠근다.
2. 송신: 자동 선택에서 한 adapter의 home 오류(symlink, 권한, walk 오류)는 그 adapter만 건너뛰고 warning으로 남긴다. 명시 ID 요청에서 해당 세션을 못 찾으면 R15대로 실패하고 adapter 오류를 원인으로 붙인다.
3. export 직후의 세션 안정성 재검사도 같은 규칙을 따른다.

완료 기준: Claude home이 손상돼도 Codex-only package가 import된다. 송신 측 Codex home이 손상돼도 Claude 세션과 workspace는 warning과 함께 pack된다.

**R18. P2: JSON 오류 code와 exit status가 어긋난다**

근거: [error.go:55](/Users/johnnybae/workspace/onthego/internal/otgerror/error.go:55) `Public`, [root.go:41](/Users/johnnybae/workspace/onthego/internal/cli/root.go:41) `ExitCode`.

재현(원본): 음수 `session-days`에서 JSON은 `E_INPUT`, exit는 1이었다. typed `E_INPUT`은 exit 2다.

결정:

1. `CodeInternal = "E_INTERNAL"`을 추가한다. `Public`은 untyped error를 `E_INTERNAL`("unexpected error; rerun without --json for details")로 매핑하고, `ExitCode`는 이를 1로 매핑한다. text 모드는 지금처럼 전체 오류를 출력한다.
2. R13의 입력 검증과 R15, R17의 세션 오류를 typed error로 만들어 `E_INTERNAL`로 떨어지는 경로를 줄인다.
3. 취소 오류는 `E_CANCELED`(exit 130)로 분리한다. 기존 `E_PRECONDITION`(exit 3)과 공유하면 code와 exit status가 어긋나므로 context 취소를 typed wrapper보다 먼저 판별한다. code와 exit status의 대응을 표 기반 테스트로 고정한다. 같은 표를 README의 exit code 절에 반영한다.

완료 기준: 모든 공개 code는 단 하나의 exit status를 가진다. 입력 오류가 아닌 실패가 `E_INPUT`으로 보고되지 않는다.

**7. 단계 5: 경량 견고화**

| ID | 결정 | 완료 기준 |
| --- | --- | --- |
| DEP | `golang.org/x/crypto`를 v0.56.0 이상으로, `github.com/klauspost/compress`를 v1.18.7 이상으로 올린다([GO-2026-6355](https://pkg.go.dev/vuln/GO-2026-6355), [GO-2026-5841](https://pkg.go.dev/vuln/GO-2026-5841)). 버전은 착수 시점에 `govulncheck`로 다시 확인한다. CI에 `govulncheck` job을 추가하고, release workflow의 action을 commit SHA로 고정한다. | source 검사에서 module 수준 보고가 해당 두 건만큼 줄어든다. CI에서 정기 실행된다. |
| S01 일부 | `applyState`의 "기존 파일 삭제 후 쓰기"를 "같은 디렉터리의 임시 파일에 쓰고 `rename`"으로 바꾼다. symlink도 임시 이름으로 만든 뒤 rename한다. 단계별 journal과 crash 재개는 보류한다(9절). | 쓰기 도중 실패해도 대상 파일이 비거나 사라진 상태로 남지 않는다. 쓰기 실패 주입 테스트로 확인한다. |
| S10 | text 출력에서 파일명과 세션 ID에 제어 문자가 있으면 `strconv.Quote` 형태로 escape한다. JSON 출력은 그대로 둔다. | newline과 ANSI escape가 든 파일명이 터미널을 조작하지 않는다. |

**8. 테스트 보강 목록**

| 경계 | 테스트 | 성공 조건 |
| --- | --- | --- |
| 관리 해제 | 정책만 바뀐 미추적 파일과 secret, 원본에서 실제 삭제한 파일 | 앞의 둘은 수신 측 보존과 안내, 마지막은 삭제 전파 |
| 파일시스템 | 대소문자 충돌, NFC/NFD 충돌, 대소문자만 바꾼 rename | 충돌은 적용 전 거부와 대상 불변, rename은 정상 적용 |
| 파일 정책 | 토큰이 든 `.npmrc`, `--include-secret .npmrc`, `src/tokenizer.ts` | 자동 선택 제외, 허용 시 Git 객체 없음, 일반 소스는 유지 |
| 정책 검증 | 잘못된 agent, schema 2의 알 수 없는 키, schema 1 파일, 잘못된 incoming 정책 | 로컬은 `E_INPUT`, incoming은 적용 전 거부, schema 1은 계속 동작 |
| package 왕복 | 고압축률 데이터, 낮춘 상한의 경계값, 서명 오류, 크기 불일치 | Create 성공분은 항상 Extract 성공, 미인증 payload는 디스크에 쓰지 않음 |
| offline 계보 | 미확인 export 3개, `gc --prune=now`, 첫 export의 귀환 | `--replace` 없이 수락, pending ref가 bundle에 없음 |
| 세션 복구 | import 실패 후 같은 package 재시도, `sessions recover`, 부분 성공 후 재시도 | workspace 편집 보존, 세션만 복구 |
| backup | lock 중 prune, 정책 파일 삭제와 손상 후 list와 undo | prune은 `E_PRECONDITION`, undo는 init 없이 동작 |
| 취소 | peer apply 무입력과 부분 입력에서 SIGINT, SIGTERM. npm launcher 단독 SIGTERM과 process group SIGTERM | 1초 안에 종료, orphan 없음, 임시 디렉터리 없음 |
| 선택과 진단 | `--no-sessions`, 명시 ID 누락, malformed 세션 inspect, adapter 하나 손상, 음수 `session-days`의 dry-run과 실제 실행 | 각 항목의 완료 기준과 일치, code와 exit 대응표와 일치 |

fuzz 대상(이번 범위에서는 선택 사항): package 경로 검증, JSONL token rewriter, manifest validator, `retained_paths` 검증.

**9. 이번 범위에서 보류하는 항목**

아래 항목은 원본 보고서의 관찰을 유지한다. 다만 이번 릴리스의 완료 조건에서는 뺀다.

| ID | 원본 우선순위 | 보류 사유 | 재검토 시점 |
| --- | --- | --- | --- |
| S01 전체 (적용 journal, crash 재개) | P1 | 파일 단위 원자적 교체(7절)로 가장 큰 위험인 "빈 파일과 사라진 파일"을 먼저 닫는다. journal은 상태 schema 변경이 필요한 별도 설계다. | 다음 minor |
| S02 나머지 (디스크 여유 검사, Git object 확장 비용) | P2 | 서명 우선 검증, 서명된 크기, stdin 상한으로 미인증 입력 경로는 닫힌다. | 대형 저장소 사용자 보고 시 |
| S03 (계층별 크기 예산, streaming) | P2 | 측정 없이 바꾸면 회귀 위험이 크다. 먼저 peak RSS를 측정한다. | benchmark full 실행 후 |
| S04 (WIP ancestry 보존량, 저장소 정리) | P2 | R02로 이미 저장된 비밀의 정리도 여기에 속한다. 계보 checkpoint 프로토콜 설계가 필요하다. | 다음 minor 최우선 |
| S05 (mtime 기반 최근 활동) | P2 | 동작 변경 시 기존 선택 결과가 달라진다. | U04와 함께 |
| S06 (사용자 설정 동시 수정 lock) | P2 | device add와 trust를 동시에 실행하는 경우로 한정된다. | U10과 함께 |
| S07 (agent metadata, capability) | P2 | U08의 native 호환성 검증과 함께 설계한다. | U08 |
| S08, S09 (인수 길이, idle timeout, peer JSON 상한) | P2 | ref가 매우 많은 저장소에서만 드러나며 재현 기록이 없다. 먼저 재현한다. | 재현 시 |
| S11 (release artifact architecture 검사) | P2 | release 경로 전용이다. 다음 release 전에 별도로 처리한다. | 다음 release 준비 |
| S12 나머지 (`BuildHandoffPlan` 등 경계 분리) | P3 | R03, R13, R16에서 필요한 공유 함수만 먼저 만든다. | U01과 함께 |
| 세션 ID가 `sessions`와 `archived_sessions`에 중복 | - | 재현한 장애가 아니라 adapter 검증 과제다. | U08 |
| U01–U12 | P1–후순위 | 제품 개선 과제다. 단, U02의 최소 형태(`sessions recover`, status 표시)는 R06에 포함했다. | 안정화 릴리스 이후 |

U 항목 원문 요약(우선순위 순): U02 복구 명령(일부 반영), U01 handoff plan, U04 status의 workspace와 session 분리, U06 `doctor <device>` 확장, U07 managed secret, backup, conflict, recovery 목록과 해제, U08 native resume fixture 확장, U05 진행률, U03 기존 clone adoption, U09 remote list와 mapping 조회, U10 pairing 상태, U11 로컬 재개 지표, U12 비동기 전달. 새 UI나 중앙 서버를 먼저 만드는 것은 현재 문제의 해결책이 아니다.

**10. 근거 부록**

**검증 환경과 실행 결과 (원본 보고서)**

| 검증 | 결과 | 해석 |
| --- | --- | --- |
| `make check` | 통과 | gofmt, vet, 전체 Go 테스트 |
| `go test -race ./...` | 통과 | 이번 실행에서 race 보고 없음 |
| 실제 OpenSSH E2E | 통과 | 임시 sshd로 pairing, push, 원격 수정, pull |
| `scripts/sigint-e2e.py` | 통과 | native 바이너리의 capture 중 SIGINT, exit 130, 재시도, 임시 데이터 정리 |
| `scripts/benchmark.py --profile smoke` | 통과 | 초기 push 1.846초, 증분 push 2.312초, return pull 1.869초. 동시에 다른 검증을 실행했으므로 기준선으로 쓰지 않는다. |
| `node npm/scripts/verify-package.mjs` | 통과 | 4개 플랫폼 바이너리와 npm 파일 목록 |
| `govulncheck@v1.9.0` | 호출 가능한 알려진 취약점 0건 | imported package 수준 1건, required module 수준 16건 별도 보고(7절 DEP) |

환경: macOS 27.0.1 arm64, Go 1.27.1, Git 2.54.0. 세션과 토큰은 합성 데이터만 사용했다.

coverage 53.7%(CLI 21.8%, gitx 14.1%, restore 30.9%)는 하위 프로세스와 다른 패키지의 통합 테스트가 합산되지 않은 값이다. 기능 검증 비율로 해석하지 않는다. `-coverpkg`와 하위 프로세스 coverage 수집을 정리한 뒤 기준선을 잡는다.

실행하지 않은 것: Linux 직접 실행, 1GiB full benchmark, 4개 플랫폼 신규 패키지 빌드, 실제 Codex와 Claude CLI의 native resume 재실행, 원격 CI 결과와 배포된 npm tarball 확인.

**개정 시 추가 검증**

- R01–R18의 코드 근거를 기준 커밋에서 다시 읽고 원인 설명이 맞는지 확인했다. 링크의 행 번호는 일부 함수 시작 위치로 보정했다.
- R19는 `internal/syncer`의 기존 helper로 임시 테스트를 만들어 재현했다. 임시 테스트는 삭제했다. 결과: 수신 측 파일 삭제, backup ref 생성, warning 없음.
- R06의 해결 전제(세션 import 멱등성)는 `planImport`의 동일 내용 skip과 append 확장 처리로 확인했다.
- R07의 완료 기준은 `fsx.Lock`이 non-blocking이고 `E_PRECONDITION`을 반환한다는 점으로 확인했다.
- R04의 판단(압축률 규칙이 실질적 보호를 하지 않음)은 `repo.bundle`이 압축률 계산에서 빠져 있고, 그 밖의 entry는 절대 상한으로 제한된다는 점에 근거한다.

**유지할 설계 (원본 3절 요약)**

Git index와 working tree 분리, `.git`과 로컬 의존성 디렉터리를 건드리지 않는 제자리 적용, age 암호화와 signed manifest의 이중 검증, Git metadata와 symlink 경로 경계, supplemental secret의 별도 암호화 백업, WIP ancestry와 lost acknowledgment 복구, 세션 충돌 시 대상 기록 보존, 알려진 path token만 치환하는 세션 바이트 보존, flock과 임시 디렉터리 lease, 개인 CLI 사용자와 사용자 소유 장치라는 제품 범위.

**위협 경계**

SSH 계정을 사용할 수 있는 주체는 그 계정에서 다른 명령도 실행할 수 있다. 따라서 pairing을 사용자 계정 안의 sandbox처럼 표현하지 않는다. 반면 파일 선택 정책, package 무결성, 평문 잔존, 미인증 입력의 자원 소모 방지는 자기 소유 장치끼리 쓰는 제품에서도 실질적인 책임이다. 이번 범위의 R02, R04, R19가 여기에 해당한다.

**11. 이번 릴리스의 완료 기준**

- 정책만 바뀐 파일은 수신 측에서 지워지지 않는다(R19).
- 대소문자와 정규화 충돌은 적용 전에 거부되고, 성공한 적용에서 서로 다른 경로가 같은 파일을 가리키지 않는다(R01).
- 확장된 secret candidate는 자동 선택되지 않고, 허용한 secret은 어느 쪽 Git 객체에도 없다. release note에 토큰 교체 안내가 있다(R02).
- `Create`가 성공한 package는 같은 버전에서 복원된다. 미인증 package는 payload를 디스크에 쓰지 않는다(R04).
- 잘못된 정책은 세션 검색 전에 실패한다(R03).
- 세션 import 실패 후 같은 package나 `sessions recover`로 세션만 복구된다(R06).
- 정책 파일이 없거나 손상돼도 backup 확인과 undo가 동작한다. prune은 lock을 따른다(R07, R08).
- 여러 미확인 export와 GC 뒤에도 정상 귀환이 수락된다(R05).
- peer apply와 npm launcher가 신호를 받으면 정해진 시간 안에 종료되고 orphan과 임시 데이터가 남지 않는다(R09, R10).
- dry-run과 실제 실행이 로컬 검증을 공유한다. 누락 세션, 제외 비밀, 부분 성공, adapter별 장애가 text와 JSON에 드러난다. 공개 오류 code와 exit status가 일대일로 대응한다(R11–R18).
- snapshot schema 3과 새 capability가 문서화되고, 구버전과의 비호환이 release note에 적혀 있다.
- macOS와 Linux CI에서 8절의 회귀 테스트가 통과한다.
