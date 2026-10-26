# page

GitHub 저장소의 이미 빌드된 정적 파일을 S3에 게시하는 웹 호스팅 서버다.
이름 규칙에 맞고 유효한 배포 manifest가 있는 `foo.app`을 발견하면
`foo.example.com`으로 제공한다. branch/tag/commit preview와 Git sync가
소유하는 보존·GC 정책을 포함한다.

payday가 생성한 리소스·서비스 인터페이스를 사용한다. 실제 제품 실행 경로는
Storage 기반 구현이며 SQL Sink나 DB를 열지 않는다. 기본 React 화면과 SQL
scaffold는 별도의 개발 예제로 남겨두었다.

## 빠른 시작

Go 1.27 이상과 Docker Compose가 필요하다. TypeScript 재생성·전체 검증에는
Node.js 24와 npm도 필요하다. Buf는 payday가 내려받는다.

```sh
go mod download
./scripts/test-s3.sh

export AWS_ACCESS_KEY_ID=page-dev
export AWS_SECRET_ACCESS_KEY=page-dev-secret
export AWS_REGION=us-east-1
export GITHUB_TOKEN=your-read-only-github-token
```

`page.yaml`의 `github` 목록에 [설정 예시](docs/mvp.md)의 소스를 추가하고 owner를
자신의 조직에 맞춘다. 대상 저장소에는
아래 manifest와 `public/index.html` 등 이미 빌드된 파일을 커밋한다.

```json
{"schemaVersion":1,"publish":{"directory":"public"}}
```

```sh
go run ./cmd/page run --config page.yaml
curl -H 'Host: foo.example.com' http://127.0.0.1:8080/
```

compose는 개발용 S3Mock을 `127.0.0.1:19090`에서 실행하고 `page`,
`page-storage-test` 버킷을 준비한다. 실제 AWS S3를 사용할 때는 버킷을 먼저
만들고 `storage.endpoint`를 비우며 AWS의 표준 자격 증명 체인을 사용한다.
S3Mock 검증은 실제 AWS의 IAM·장애·일관성 검증을 대신하지 않는다.

## 실행 모드

컨테이너 빌드·이미지 발행과 Kubernetes 실행 조건은 [컨테이너 배포](docs/containers.md)를 참고한다.

```sh
go run ./cmd/page serve             # 게시 상태 읽기 + HTTP/읽기 전용 gRPC
go run ./cmd/page sync              # GitHub 발견·게시·보존·GC polling
go run ./cmd/page sync --once       # 한 번 동기화하고 종료
go run ./cmd/page run               # serve와 sync를 함께 실행
go run ./cmd/page status            # 게시된 사이트·SHA·운영/preview URL 조회
go run ./cmd/page config            # 토큰을 가린 설정 확인
go run ./cmd/page version
```

`--config`는 명령 앞뒤에 둘 수 있으며 기본 파일은 `page.yaml`이다. `serve`는
GitHub에 접속하지 않는다. sync가 중단되어도 이미 게시한 사이트는 계속 제공한다.

## 라우팅과 배포 제어

게시 디렉터리 루트의 `pages.json`으로 rewrite·내부 redirect·clean URL·SPA fallback·
응답 헤더·404 문서를 설정한다. 설정은 파일 목록과 함께 검증하고 릴리스에 고정한다.
파일이 없으면 기본 정적 파일·directory index 동작을 사용한다.
[설정 예시](examples/pages.json)와 [라우팅 규칙](docs/mvp.md#pagesjson-라우팅)을 참고한다.

```sh
go run ./cmd/page routes validate ./public
go run ./cmd/page routes test ./public /docs/start
go run ./cmd/page routes test --navigation=false ./public /assets/missing.js
```

다음 명령은 Storage에 남아 있는 릴리스를 조회하거나 운영 배포의 제어 의도를 기록한다.
`RELEASE`에는 `releases`가 출력한 내부 ID를 사용한다.

```sh
go run ./cmd/page releases foo
go run ./cmd/page promote foo RELEASE --wait 90s
go run ./cmd/page rollback foo --wait 90s
go run ./cmd/page pin foo             # 현재 운영 배포 고정
go run ./cmd/page pause foo           # 운영 배포 자동 전환 일시 중지
go run ./cmd/page resume foo          # 소스 정책에 따른 자동 전환 재개
go run ./cmd/page remove foo          # 운영·preview 공개 중단
```

제어 명령은 `control.json`에 요청을 저장하고 소유 sync가 다음 실행에서 적용한다.
`--wait`는 적용 확인까지 기다린다. sync가 실행 중이 아니면 요청은 대기 상태다.
`status`의 `controlApplied`로 적용 여부를 확인한다. promote·rollback·pin은 resume까지
운영 배포를 고정하며 pause 중에도 preview는 갱신한다. 관리 명령에는 Storage 쓰기
권한이 필요하다. `--config`, `--source`, `--wait`는 사이트 이름 앞뒤에 둘 수 있다.

배포 전환 뒤에는 이전 운영 릴리스의 `assets.immutable` 파일만 `asset_retention`
동안 기존 URL로 제공한다. 기본 유예는 24시간이며 HTML과 게시되지 않은 preview
파일은 포함하지 않는다. 파일명에 콘텐츠 해시를 넣고 같은 URL의 내용을 바꾸지 않는다.
자세한 제어·자산 보존 의미론은 [운영 안내](docs/mvp.md)에 기록했다.

## preview와 읽기 API

preview는 기본적으로 `<별칭>--foo.example.com`을 사용한다.
`preview_style: nested`면 `<별칭>.foo.example.com`이며 앱별 DNS/TLS가 필요하다.
별칭에는 ref 종류 접두사와 충돌 방지 해시가 포함된다. 게시된 사이트의 전체 SHA와
정확한 운영·preview URL은 서버를 시작하지 않고도 Storage에서 조회할 수 있다.

```sh
go run ./cmd/page status --config examples/github.yaml
```

출력 URL은 외부 TLS 종료를 전제한 HTTPS 주소다. 로컬 개발 중에는 위의
`curl -H 'Host: ...' http://127.0.0.1:8080/` 방식으로 확인한다.

읽기 전용 gRPC API도 사이트와 배포 metadata를 제공한다.

```sh
grpcurl -plaintext -d '{}' 127.0.0.1:50051 app.SiteService/List
grpcurl -plaintext -d '{}' 127.0.0.1:50051 app.DeploymentService/List
```

API는 기본 loopback에 바인딩하고 reflection을 제공한다. 생성된 Get/List를
구현하며 변경 RPC와 관리 콘솔은 제공하지 않는다.

## 검증과 개발

```sh
npm ci --prefix ts
./scripts/check.sh
./scripts/test-s3.sh
```

`check.sh`는 payday 진단·생성물 일치·Go 빌드/테스트·TypeScript 빌드를 확인한다.
S3 API 계약과 GitHub fixture → 실제 S3 API → HTTP 서빙·preview·GC의 통합 검증은
별도 스크립트로 실행한다. CI에서도 개발용 S3 서비스를 시작해 두 검증을 실행한다.

```sh
go tool pd gen --ts .
go mod tidy
```

스키마 변경 뒤에는 위 명령으로 재생성한다. 생성된 `*.pb.go`, `*.g.go`,
`server/`, `internal/ent/`, `ts/gen/`는 직접 수정하지 않는다. payday는
`v0.0.0-20261025134239-7ae6f76a47ac`에 고정되어 있다.

[MVP 설정·운영 안내](docs/mvp.md)에 데이터 구조와 preview/GC 의미론, DNS/TLS,
검증 범위를 기록했다. 기본 MVP는 [계획 #2](https://github.com/lesomnus/page/issues/2),
라우팅·배포 제어·이전 자산 처리는 [확장 계획 #14](https://github.com/lesomnus/page/issues/14)와
sub-issue #15–#18에서 추적한다. [기존 설계 노트 #1](https://github.com/lesomnus/page/issues/1)은
더 넓은 후속 구상을 담고 있다.
