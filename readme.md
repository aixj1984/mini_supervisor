## 服务设置
```
services:
  - name: myapp
    cmd: ["/usr/local/bin/myapp", "--port", "8080"]
    work_dir: /usr/local/bin
    auto: false
  - name: pingtest
    cmd: ["ping", "www.baidu.com", "-t"]
    work_dir:
    auto: false
  - name: telnet
    cmd: ["./test.exe"]    # windows下，必须设置./
    work_dir: "E:/01project/00go/mini_supervisor"  #  E:\\01project\\00go\\mini_supervisor
    auto: true
```
