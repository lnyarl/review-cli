# review-cli

AI 에이전트가 호출하는 코드 리뷰 CLI입니다. 리뷰 대상 코드를 `.git` 없이 꺼내고, 읽기 도구만 허용한 Claude Code로 리뷰한 뒤, 리뷰 실행·지적·판정을 로컬 SQLite ledger에 쌓습니다. 쌓인 판정과 고정된 평가 suite로 리뷰 품질을 비교하며 개선하는 것이 목적입니다.

모든 명령은 대화형 입력 없이 JSON 하나를 stdout에 출력합니다. 종료 코드는 0 성공, 1 실패(실패하거나 미완료인 리뷰도 출력·기록), 2 사용법 오류입니다. 터미널에서 `review --help`를 실행하면 사용법을 텍스트로 보여 줍니다.

## 설치

필요한 것: Go 1.26 이상, git 2.38 이상, 로그인된 [Claude Code](https://claude.com/claude-code) CLI(`claude`). API 키가 아니라 Claude Code 로그인으로 실행합니다.

```sh
scripts/install-review.sh                          # ~/.local/bin/review
PREFIX=/usr/local/bin scripts/install-review.sh    # 다른 위치
```

다시 실행하면 현재 체크아웃의 코드로 다시 설치합니다.

## 데이터 위치

| 항목 | 기본 위치 | 변경 |
| --- | --- | --- |
| ledger(SQLite) | `<사용자 설정 디렉터리>/review-cli/review.sqlite` (macOS: `~/Library/Application Support/review-cli/`) | `--db`, `REVIEW_DB` |
| 리뷰 작업공간 | `<사용자 캐시 디렉터리>/review-cli/work`, 실행 후 삭제 | `REVIEW_WORK_ROOT`, `--keep-workspace` |
| 모델 | `claude-sonnet-5-5` | `--model`, `REVIEW_MODEL` |

## 리뷰 실행

```sh
review run --repo <저장소> <대상> [--issue-file <요구사항.md>] [--effort auto|low|high]
```

| 대상 | 인자 |
| --- | --- |
| 임의 범위(staged, 파일 일부, 수정안) | `--base <commit> --patch <파일 또는 ->` (`--label`로 범위를 기록) |
| commit 범위 | `--base <rev> --head <rev>` |
| Azure DevOps PR | `--pr <번호> --collector-db <collector.sqlite>` (수집 DB의 PR 기록에서 마지막 merge target/source commit을 찾음. PR 저장소의 sshUrl이 로컬 origin과 같아야 함) |
| 커밋되지 않은 변경 | `--working-tree [--paths a,b]` |

```sh
git diff --cached --binary | review run --repo . --base HEAD --patch - --label staged
```

리뷰 한 번은 여러 패스로 이루어집니다.

| 순서 | 패스 | 내용 |
| --- | --- | --- |
| 1 | 사실 수집(모델 없음) | diff에서 트리거 위치를 찾고 결정적 점검을 돌립니다. 결과는 작업공간의 `facts.md`로 모든 패스에 전달됩니다. |
| 2 | 결함 패스 | 전체 리뷰. issue가 "범위 밖·알려진 간극"이라 적은 결함도 `scope=out_of_scope`로 보고하고, 같은 파일의 이전 지적을 다시 확인합니다. |
| 3 | 트리거 점검 | 트리거마다 좁은 질문 하나를 독립 호출로 확인합니다. 확인한 대상과 근거(파일:줄)가 필수입니다. |
| 4 | 구조 패스 | 계층·책임 위치, 이웃 구현과의 차이, 중복, 팀 규칙 위반. 지적은 `category=structure`로 따로 셉니다. |

| 트리거 | diff에서 찾는 것 | 확인하는 것 |
| --- | --- | --- |
| `lock_reload` | `with_lock`, `lock!`, `reload`, `FOR UPDATE` | select 별칭·메모리 속성 소실, 잠금 전에 읽은 객체 저장, 기존 잠금 관례 |
| `state_transition` | 전이·상태 변경 | 그 상태로 들어가는 모든 경로, 옮긴 전이의 다른 콜백, 같은 이벤트 재수신 |
| `async_hop` | `perform_at`, `perform_async`, 메시지 발행 등 | 같은 상태를 읽는 작업과의 실행 순서, 지연·재시도·중복, 영원히 남는 대기 |
| `guard_removed` | 지운 가드·대기 조건 | 그 조건이 간접적으로 보장하던 것 |
| `exception_status` | 새 예외·상태 코드 | rescue_from·renderer를 거친 실제 응답 |
| `removed_contract` | 지운 응답 키·파라미터·메서드·export | 남은 사용처(연관 저장소 포함), 옛/새 클라이언트 조합 |
| `input_parsing` | `Integer(`, `to_i`, `JSON.parse` 등 | 경계값과 예외가 rescue 범위 안인지 |
| `new_tests_ci` | 새 테스트 파일 | CI가 실제로 그 테스트를 선택해 실행하는지 |

결정적 점검(`origin=check`): 검사 억제 주석 추가(`rubocop:disable`, `@ts-ignore` 등), 지운 정의를 아직 참조하는 코드, `--merge-with`로 준 열린 변경과의 충돌.

트리거 점검이 결론과 근거를 내지 못하면 리뷰는 `complete=false`가 되고 종료 코드 1로 끝납니다.

`--effort auto`(기본)는 결함 패스에 적용됩니다. 변경 파일 10개 이상, 추가 500줄 이상, migration·캐시·잠금·상태 전이·worker·job 경로를 건드리면 high, 아니면 low로 리뷰하고, low 결과에 severity high 지적이 있으면 high로 다시 리뷰합니다. 트리거 점검과 구조 패스는 medium입니다.

| 옵션 | 용도 |
| --- | --- |
| `--context-file PATH` (반복) | 명세·팀 규칙·CI 결과 같은 자료(파일이나 디렉터리, 텍스트만) |
| `--with-repo NAME=PATH[@REV]` (반복) | 연관 저장소를 `./repos/NAME`에 읽기 전용으로 넣고, 지운 키·정의를 그 저장소에서 검색 |
| `--merge-with REV` (반복) | 같은 파일을 건드리는 열린 변경과의 충돌을 `git merge-tree`로 확인(commit 범위 대상) |
| `--passes defect,triggers,structure` | 일부 패스만 실행 |
| `--targeted` | 특정 주장만 확인하는 리뷰. 리뷰 범위 계산에서 빠지고 기본으로 결함 패스만 실행 |

결과의 `run.findings[].id`(예: `3b8bca43-2`)가 판정에 쓰는 지적 ID입니다. 지적마다 `origin`(`model`, `check`, `human`, `agent`), `pass`, `category`, `scope`가 붙습니다.

## 리뷰 범위 확인

```sh
review coverage --repo <저장소> --base <target> --head <PR head>
```

변경 파일마다 지금 내용(git blob hash)을 리뷰한 기록이 있는지 보여 줍니다. 리뷰 뒤에 다시 바뀐 파일은 `all_covered=false`로 드러납니다. 커밋 전에 patch나 작업 트리로 리뷰한 내용도 같은 내용이면 리뷰된 것으로 셉니다. `--targeted` 리뷰와 eval은 세지 않습니다.

## 조회와 판정

```sh
review list [--limit N]
review show <run_id>
review judge <finding_id> 결함|유용|오탐|미확정|중복 [--note "이유"] [--actor 이름]
review add-finding <run_id> --file <경로> --line <줄> --title <제목> [--origin human|agent] [--verdict 결함|유용]
```

| 판정 | 의미 |
| --- | --- |
| 결함 (`defect`) | 실제로 발생하고 리뷰 당시 고쳤어야 할 문제 |
| 유용 (`useful`) | 결함은 아니지만 리뷰로서 가치 있는 지적 |
| 오탐 (`false_positive`) | 발생하지 않음, 이미 처리됨, 의도된 동작 |
| 미확정 (`unsure`) | 코드만으로 판단 어려움 |
| 중복 (`duplicate`) | 같은 리뷰의 다른 지적과 같은 내용 |

판정은 지우지 않고 쌓이며 최신 판정이 현재 값입니다. `add-finding --origin human`은 리뷰가 놓친 지적을 그 리뷰에 넣고 판정(기본 결함)을 함께 기록합니다. `--origin agent`는 다른 AI가 찾은 지적으로, 사람이 판정합니다.

## 회귀 평가

```sh
review eval --suite <suite.json> [--parallel N] [--effort auto|low|high] [--passes ...]
```

suite의 모든 사례를 다시 리뷰한 뒤, 기대 지적마다 같은 원인(같은 발생 조건과 영향)을 짚은 지적이 있는지 모델이 판정합니다(`matched`, `partial`). 같은 파일 ±30줄 안의 지적 수(`near_hits`)는 참고로만 남깁니다. 사례에 `cutoff`를 주면 그 이전에 기록된 지적만 다시 확인 대상으로 넘겨, 나중에 알게 된 내용이 평가에 새지 않게 합니다.

```json
{
  "name": "my-suite",
  "repo": "/path/to/repo",
  "cases": [
    {
      "id": "PR123", "kind": "defect", "base": "<merge base>", "head": "<reviewed commit>",
      "cutoff": "2026-01-01T00:00:00Z", "issue_file": "issues/PR123.md",
      "with_repos": [{"name": "client", "path": "/path/to/client", "rev": "<commit>"}],
      "context": ["docs/spec.md"],
      "expected": [{"file": "app/x.rb", "line": 42, "summary": "…", "detail": "리뷰 원문"}]
    }
  ]
}
```

## 격리

리뷰 모델은 `claude --print --restricted`로 실행합니다. 명령 실행 도구는 없고 `Read`, `Grep`, `Glob`만 허용하며, 사용자·프로젝트 설정과 MCP 서버를 불러오지 않습니다. 파일 도구는 작업공간 안으로 제한됩니다. sandbox가 막은 도구 호출은 `run.warnings`에 기록됩니다.
