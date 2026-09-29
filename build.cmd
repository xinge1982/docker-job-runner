SET mypath=%~dp0
set GOOS=linux
set GOARCH=amd64
set CGO_ENABLED=0
set GOPROXY=https://goproxy.io
FOR /F "tokens=*" %%g IN ('git rev-list -1 HEAD') do (SET GIT_COMMIT=%%g)
SET BUILD_DATE=%date:~0,4%%date:~5,2%%date:~8,2%
if not exist %mypath%build\ mkdir %mypath%build\


REM ********************************************
REM 编译cmds

CD cmd
FOR /D %%G in ("*") DO (
    cd %%~nxG
    go build  -ldflags "-s -w -X main.GitCommit=%GIT_COMMIT%_dev -X main.BuildDate=%BUILD_DATE%" -o %%~nxG
    copy /Y %%~nxG %mypath%build\
    del %%~nxG
    cd ..
)

cd %mypath%
