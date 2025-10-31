package flags

// bz: global information

// some package names for git repos:
// "google.golang.org/grpc"
// "github.com/pingcap/tidb"
// "k8s.io/kubernetes"
// "github.com/ethereum/go-ethereum"

var ExcludedPkgs = []string{ //bz: excluded a lot of default constraints -> only works if a.config.Level == 1 or turn on DoCallback (check a.createForLevelX() for details)
	//"runtime",
	//"reflect", -> only consider when turn on a.config.Reflection or analyzing tests
	//"os",

	//bz: check /_founds/sum.md for the following exclusions -> create too many interface related type of pointers with pts > 100
	"fmt",
	"errors", //there are so many wrappers of errors ...
	// grpc
	"google.golang.org/grpc/grpclog",
	"google.golang.org/grpc/internal/binarylog",
	// etcd
	"go.etcd.io/etcd/raft/raftpb",
	"go.etcd.io/etcd/lease/leasepb",
	"go.etcd.io/etcd/auth/authpb",
	"go.etcd.io/etcd/mvcc/mvccpb",
	"go.etcd.io/etcd/etcdserver/etcdserverpb",
	"go.etcd.io/etcd/pkg/logutil",
	"go.etcd.io/etcd/pkg/debugutil",
	"go.etcd.io/etcd/raft",
	"go.uber.org/zap",
	"github.com/coreos/pkg/capnslog",
	"go.etcd.io/etcd/etcdserver/api/snap/snappb",
	// syncthing
	"github.com/syncthing/syncthing/cmd/syncthing/proto",
	"github.com/syncthing/syncthing/lib/config",
	// prometheus
}
