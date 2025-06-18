echo "start build....."
:: 转中文输出
chcp 65001

SET OutFile=mini_supervisor

if exist "%OutFile%" (
    echo "删除旧文件"
    del  "%OutFile%"
)

echo 当前盘符和路径：%~dp0

SET CGO_ENABLED=0
set GOARCH=amd64
set GOOS=linux

go build -o "%OutFile%"

SET CGO_ENABLED=0
SET GOOS=windows
SET GOARCH=amd64

