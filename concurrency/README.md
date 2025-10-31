# GoGuard

_Note: always use go1.17 to compile and run_

Please refer to https://go.dev/dl/ about downloading a binary release of go1.17.

## How to use?
Obtaint the project:
```bash
git clone https://github.com/bozhen-liu/GoGuard.git
cd GoGuard
git checkout cshba
go build
```
Then you will obtain an executable file, let's say name it `goguard`.
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
and the following command is to run our blocking bug detection together with data race detection:
```bash
./goguard -doSeq -doTests -doBlocking -doRace $path_to_your_gitcloned_gobench/gobench/goker/blocking/cockroach/10214/cockroach10214_test.go
```
where `-doSeq` is a flag for pointer analysis, `-doTests` is a flag when we consider tests as analysis entries.