## github-profile-comments

### 목표:

GitHub 프로필에 댓글 기능을 추가하여 사용자들이 프로필에 댓글을 남길 수 있도록 합니다.

### 사용법:

1. [여기](https://comment.injunweb.com/api/login)로 이동하여 회원가입을 진행합니다.
2. 프로필 README에 다음 코드를 추가합니다:

    ```markdown
    [![Comments](https://comment.injunweb.com/api/user/{깃허브 아이디}/svg?theme={테마})](https://comment.injunweb.com/{깃허브 아이디})
    ```

    여기서 `{깃허브 아이디}`는 본인의 깃허브 아이디로 대체해야 하며, `{테마}`는 사용하고자 하는 테마 이름으로 대체되어야 합니다. 가능한 테마 값은 "black", "white", "transparent"입니다.

    예를 들어, 다음과 같이 사용할 수 있습니다:

    ```markdown
    [![Comments](https://comment.injunweb.com/api/user/in-jun/svg?theme=black)](https://comment.injunweb.com/in-jun)
    ```

    테마 파라미터를 작성하지 않는 경우, 디폴트 테마 값은 `white`입니다.

### 핵심 기능:

1. GitHub 유저의 아이디를 이용하여 고유한 댓글창 URL을 생성합니다.
2. 사용자가 README 링크를 클릭하면 댓글을 추가하는 링크로 이동하고, 해당 페이지에서 댓글을 작성할 수 있습니다.
3. 댓글을 작성할 때는 댓글과 함께 유저의 로그인 아이디를 저장합니다.
4. 각 유저는 한 개의 댓글만 작성할 수 있습니다.
5. 댓글은 SVG 형식으로 표시되며, 댓글을 추가할 때마다 SVG가 업데이트됩니다.

### 프로젝트 환경:

-   언어: Go 언어
-   데이터베이스: MySQL
-   프레임워크: Gin

### 결과:

-   black 테마:

    [![Comments](https://comment.injunweb.com/api/user/in-jun/svg?theme=black)](https://comment.injunweb.com/in-jun)

-   white 테마:

    [![Comments](https://comment.injunweb.com/api/user/in-jun/svg?theme=white)](https://comment.injunweb.com/in-jun)

-   transparent 테마:

    [![Comments](https://comment.injunweb.com/api/user/in-jun/svg?theme=transparent)](https://comment.injunweb.com/in-jun)
