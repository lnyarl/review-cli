package review

import (
	_ "embed"
	"fmt"
	"strings"
)

var (
	//go:embed findings.schema.json
	findingsSchema string
	//go:embed check.schema.json
	checkSchema string
	//go:embed judge.schema.json
	judgeSchema string
)

const workspaceGuide = `- ./issue.md: 이 변경의 목적
- ./change.diff: 리뷰 대상 변경
- ./repo: 변경이 적용된 시점의 전체 코드
- ./facts.md: 코드로 미리 수집한 사실(트리거 위치, 새 테스트와 CI 설정, 지운 정의의 남은 참조, 충돌, 연관 저장소 검색 결과)
- ./context: 요구사항·명세·팀 규칙·CI 결과 같은 추가 자료(있을 때)
- ./repos: 연관 저장소 코드(있을 때). 이 변경과 주고받는 계약을 확인할 때 읽는다.
`

const fileRules = `파일 도구에는 작업 디렉터리 기준 상대 경로(예: repo/app/models/x.rb)를 쓴다. 작업 디렉터리 밖의 파일은 읽지 않는다. 파일을 수정하지 않는다.
`

const findingRules = `각 지적에는 발생 조건, 코드 경로, 영향, 근거 위치(repo 기준 파일:줄)를 적는다. 코드로 확인하지 못한 추정은 confidence를 low로 둔다.
`

const defectPrompt = `너는 코드 리뷰어다.

` + workspaceGuide + `- ./prior_findings.md: 같은 파일에 대한 이전 리뷰 지적(있을 때)

이 변경이 만들었거나 드러낸 결함을 찾아라. 대상은 잘못된 동작, 데이터 정합성, 동시성, 보안, 성능 회귀, 계약 불일치다. 스타일·취향·단순 질문은 제외한다.
- issue가 "알려진 간극", "범위 밖", "이번에 하지 않음"이라고 적은 문제도 결함이면 보고하고 scope를 out_of_scope로 둔다. 나머지는 in으로 둔다.
- prior_findings.md의 지적은 이 변경에서 고쳐졌는지 다시 확인하고, 여전히 유효하면 다시 보고한다.
` + findingRules + `결함이 없으면 findings를 빈 배열로 둔다.
` + fileRules

// checkPrompt asks one narrow question. Evidence is required so an unanswered check is visible, not skipped.
func checkPrompt(t Trigger) string {
	return fmt.Sprintf(`너는 코드 리뷰어다. 이번 호출은 점검 하나만 한다.

점검: %s
질문: %s
change.diff에서 찾은 해당 위치:
%s

자료:
%s
질문에 답하는 데 필요한 코드를 끝까지 따라가 확인하라. 확인한 대상(targets_checked)과 근거(evidence: repo 기준 파일:줄과 그 줄이 보여 주는 사실)를 반드시 적는다.
conclusion: 결함이 있으면 issue, 확인했고 문제가 없으면 no_issue, 필요한 정보가 없어 판단할 수 없으면 cannot_determine(not_checked에 부족한 정보를 적는다).
issue이면 findings에 결함을 적는다. scope는 issue가 범위 밖이라고 적은 문제일 때만 out_of_scope다.
%s%s`, t.Name, t.Question, "- "+strings.Join(t.Locations, "\n- "), workspaceGuide, findingRules, fileRules)
}

const structurePrompt = `너는 코드 구조 리뷰어다. 결함이 아니라 이 변경이 저장소의 구조와 관례에 맞는지 본다.

` + workspaceGuide + `
./facts.md의 "구조" 절에 새 파일과 계층, 같은 디렉터리의 이웃 파일, 같은 이름의 기존 정의가 있다. ./repo 안의 AGENTS.md, CLAUDE.md, README와 ./context의 규칙·설계 문서도 기준이다.
볼 것: 책임이 놓인 계층(controller, service, repository, model, worker 등), 이웃 구현과 다른 방식, 기존 코드와 겹치는 구현, 한 번만 쓰는 추상화, 팀 규칙 위반, 이름.
결함이 아니면 severity는 low로 둔다. 계층 위반처럼 실제 문제로 이어지는 경우만 올린다. evidence에는 비교한 기존 파일:줄을 적는다. scope는 in으로 둔다.
지적이 없으면 findings를 빈 배열로 둔다.
` + fileRules

const judgePrompt = `./judge.json에 사람 리뷰 지적(expected)과 AI 리뷰 지적(findings)이 있다.
각 expected마다 같은 원인, 즉 같은 발생 조건과 영향을 짚은 finding이 있는지 판정하라. 위치가 같아도 다른 문제면 같은 원인이 아니다.
verdict: same_cause(원인과 영향이 같음), partial(관련 증상만 짚었거나 원인이 다름), none.
finding_ids에는 근거가 된 finding id를 적는다. 다른 파일은 읽지 않아도 된다.
` + fileRules
