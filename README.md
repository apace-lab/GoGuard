# GoGuard: Efficient Static Blocking Bug Detection for Go

_Note: always use go1.17 to compile and run_

Please refer to https://go.dev/dl/ about downloading a binary release of go1.17.

## How to use?
Obtaint the project:
```bash
git clone https://github.com/apace-lab/GoGuard.git
cd GoGuard
git checkout cshba
go build
```
Then you will obtain an executable file, let's say we name it `goguard`.
You can also config the run configuration if you use GoLand or VSCode.

We use GoBench https://github.com/timmyyuan/gobench.git as our benchmark to run our tool,
where `gobench/goker/blocking` contains the go programs with blocking bugs, and
`gobench/goker/nonblocking` contains the go programs with nonblocking bugs, e.g., data races.
Please git clone GoBench and use the programs in directory `gobench/goker/` as input for our tool.

Take the go program `gobench/goker/blocking/cockroach/10214/cockroach10214_test.go` as an example.
The following command is to run our blocking bug detection on this file:
```bash
./goguard -doSeq -doTests -doBlocking $path_to_your_gitcloned_gobench/gobench/goker/blocking/cockroach/10214/cockroach10214_test.go
```
For data race detection, the following command runs GoRace on programs in `gobench/goker/nonblocking`:
```bash
./gorace -doSeq -doTests -doRace -Plus $path_to_your_gitcloned_gobench/gobench/goker/nonblocking/etcd/6708/etcd6708_test.go
```
where `-doSeq` is a flag for pointer analysis, `-doTests` is a flag when we consider tests as analysis entries, and `-Plus` enables RFG+ for comprehensive happens-before analysis.


## GoGuard and GoRace Implementation
The implementation of GoGuard and GoRace is at `apace-lab/GoGuard/concurrency`, which is based on Go Tools from Google.


## Origin-Sensitive Pointer Analysis for Go
The other folders in this repo implements an origin-sensitive pointer analysis for Go.
We git cloned from https://github.com/golang/tools, developed from commit 146a0deefdd11b942db7520f68c117335329271a (around v0.5.0-pre1).

The default version of go pointer analysis algorithm (v0.5.0-pre1) is at ```go_tools/go/pointer_default```. Our origin implementation is at ```go_tools/go/pointer```.

The concept of origin comes from this paper:

```
Liu, Bozhen, Peiming Liu, Yanze Li, Chia-Che Tsai, Dilma Da Silva, and Jeff Huang. "When threads meet events: efficient and precise static race detection with origins." In Proceedings of the 42nd ACM SIGPLAN International Conference on Programming Language Design and Implementation, pp. 725-739. 2021.
```

We treat a go routine instruction as an origin entry point, and all variables/function calls inside this go rountine share the same context as their belonging go routine.
