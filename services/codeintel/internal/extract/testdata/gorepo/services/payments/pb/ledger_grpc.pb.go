package pb

import "google.golang.org/grpc"

var Ledger_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "payments.v1.Ledger",
	HandlerType: (*interface{})(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "PostEntry", Handler: nil},
		{MethodName: "GetBalance", Handler: nil},
	},
	Streams: []grpc.StreamDesc{{StreamName: "WatchEntries"}},
}

func RegisterLedgerServer(s grpc.ServiceRegistrar, srv interface{}) {}
