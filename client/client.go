package client

import (
	"context"
	"fmt"
	"github.com/nervosnetwork/ckb-sdk-go/v2/rpc"
	"github.com/nervosnetwork/ckb-sdk-go/v2/types"
	"log"
	"math/big"
	gpchannel "perun.network/go-perun/channel"
	"perun.network/go-perun/client"
	gpwallet "perun.network/go-perun/wallet"
	"perun.network/go-perun/watcher/local"
	"perun.network/go-perun/wire"
	"perun.network/go-perun/wire/net/simple"
	"perun.network/perun-ckb-backend/backend"
	"perun.network/perun-ckb-backend/channel/adjudicator"
	"perun.network/perun-ckb-backend/channel/asset"
	"perun.network/perun-ckb-backend/channel/funder"
	ckbclient "perun.network/perun-ckb-backend/client"
	"perun.network/perun-ckb-backend/wallet"
	"perun.network/perun-ckb-backend/wallet/address"
	vc "perun.network/perun-demo-tui/client"
	"polycry.pt/poly-go/sync"
)

type PaymentClient struct {
	observerMutex sync.Mutex
	balanceMutex  sync.Mutex
	observers     []vc.Observer
	Channel       *PaymentChannel
	Name          string
	balance       *big.Int
	Account       *wallet.Account
	wAddr         wire.Address
	Network       types.Network
	currency      gpchannel.Asset
	PerunClient   *client.Client

	channels  chan *PaymentChannel
	rpcClient rpc.Client
}

func NewPaymentClient(
	name string,
	network types.Network,
	deployment backend.Deployment,
	bus wire.Bus,
	rpcUrl string,
	account *wallet.Account,
	wallet *wallet.EphemeralWallet,
) (*PaymentClient, error) {
	backendRPCClient, err := rpc.Dial(rpcUrl)
	if err != nil {
		return nil, err
	}
	ckbClient, err := ckbclient.NewClient(backendRPCClient, deployment)
	if err != nil {
		return nil, err
	}
	f := funder.NewDefaultFunder(ckbClient, deployment)
	a := adjudicator.NewAdjudicator(ckbClient)
	watcher, err := local.NewWatcher(a)
	if err != nil {
		return nil, err
	}
	wAddr := simple.NewAddress(account.Address().String())
	perunClient, err := client.New(wAddr, bus, f, a, wallet, watcher)
	if err != nil {
		return nil, err
	}

	balanceRPC, err := rpc.Dial(rpcUrl)
	if err != nil {
		return nil, err
	}
	p := &PaymentClient{
		Name:        name,
		balance:     big.NewInt(0),
		Account:     account,
		wAddr:       wAddr,
		Network:     network,
		currency:    asset.CKBAsset,
		PerunClient: perunClient,
		channels:    make(chan *PaymentChannel, 1),
		rpcClient:   balanceRPC,
	}

	go p.PollBalances()
	go perunClient.Handle(p, p)
	return p, nil
}

// WalletAddress returns the wallet address of the client.
func (p *PaymentClient) WalletAddress() gpwallet.Address {
	return p.Account.Address()
}

func (p *PaymentClient) Register(observer vc.Observer) {
	p.observerMutex.Lock()
	defer p.observerMutex.Unlock()
	p.observers = append(p.observers, observer)
	if p.Channel != nil {
		observer.UpdateState(FormatState(p.Channel, p.Channel.State(), p.Network))
	}
	observer.UpdateBalance(FormatBalance(p.GetBalance()))
}

func (p *PaymentClient) GetBalance() *big.Int {
	p.balanceMutex.Lock()
	defer p.balanceMutex.Unlock()
	return new(big.Int).Set(p.balance)
}

func (p *PaymentClient) Deregister(observer vc.Observer) {
	p.observerMutex.Lock()
	defer p.observerMutex.Unlock()
	for i, o := range p.observers {
		if o.GetID().String() == observer.GetID().String() {
			p.observers[i] = p.observers[len(p.observers)-1]
			p.observers = p.observers[:len(p.observers)-1]
		}

	}
}

func (p *PaymentClient) NotifyAllState(from, to *gpchannel.State) {
	p.observerMutex.Lock()
	defer p.observerMutex.Unlock()
	str := FormatState(p.Channel, to, p.Network)
	for _, o := range p.observers {
		o.UpdateState(str)
	}
}

func (p *PaymentClient) NotifyAllBalance(bal int64) {
	str := FormatBalance(new(big.Int).SetInt64(bal))
	for _, o := range p.observers {
		o.UpdateBalance(str)
	}
}

func (p *PaymentClient) DisplayName() string {
	return p.Name
}

func (p *PaymentClient) DisplayAddress() string {
	addr, _ := address.AsParticipant(p.Account.Address()).ToCKBAddress(p.Network).Encode()
	return addr
}

func (p *PaymentClient) WireAddress() wire.Address {
	return p.wAddr
}

// OpenChannel opens a new channel with the specified peer and funding.
func (p *PaymentClient) OpenChannel(peer wire.Address, amount float64) {
	// We define the channel participants. The proposer always has index 0. Here
	// we use the on-chain addresses as off-chain addresses, but we could also
	// use different ones.
	log.Println("OpenChannel called")
	participants := []wire.Address{p.WireAddress(), peer}

	// We create an initial allocation which defines the starting balances.
	initAlloc := gpchannel.NewAllocation(2, p.currency)
	initAlloc.SetAssetBalances(p.currency, []gpchannel.Bal{
		CKByteToShannon(big.NewFloat(amount)), // Our initial balance.
		CKByteToShannon(big.NewFloat(amount)), // Peer's initial balance.
	})
	log.Println("Created Allocation")

	// Prepare the channel proposal by defining the channel parameters.
	challengeDuration := uint64(10) // On-chain challenge duration in seconds.
	proposal, err := client.NewLedgerChannelProposal(
		challengeDuration,
		p.Account.Address(),
		initAlloc,
		participants,
	)
	if err != nil {
		panic(err)
	}

	log.Println("Created Proposal")

	// Send the proposal.
	ch, err := p.PerunClient.ProposeChannel(context.TODO(), proposal)
	if err != nil {
		panic(err)
	}

	log.Println("Sent Channel")

	// Start the on-chain event watcher. It automatically handles disputes.
	p.startWatching(ch)

	log.Println("Started Watching")

	p.Channel = newPaymentChannel(ch, p.currency)
	p.Channel.ch.OnUpdate(p.NotifyAllState)
	p.NotifyAllState(nil, ch.State())
}

// startWatching starts the dispute watcher for the specified channel.
func (p *PaymentClient) startWatching(ch *client.Channel) {
	go func() {
		err := ch.Watch(p)
		if err != nil {
			fmt.Printf("Watcher returned with error: %v", err)
		}
	}()
}

func (p *PaymentClient) SendPaymentToPeer(amount float64) {
	if !p.HasOpenChannel() {
		return
	}
	p.Channel.SendPayment(amount)
}

func (p *PaymentClient) Settle() {
	if !p.HasOpenChannel() {
		return
	}
	p.Channel.Settle()
}

func (p *PaymentClient) HasOpenChannel() bool {
	return p.Channel != nil
}

// AcceptedChannel returns the next accepted channel.
func (p *PaymentClient) AcceptedChannel() *PaymentChannel {
	p.Channel = <-p.channels
	p.Channel.ch.OnUpdate(p.NotifyAllState)
	p.NotifyAllState(nil, p.Channel.ch.State())
	return p.Channel
}
