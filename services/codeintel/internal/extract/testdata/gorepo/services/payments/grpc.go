package payments

import (
	"google.golang.org/grpc"

	pb "example.com/payments/pb"
)

func serve() {
	s := grpc.NewServer()
	pb.RegisterLedgerServer(s, &ledger{})
}

type ledger struct{}

func (l *ledger) PostEntry() error { return nil }
