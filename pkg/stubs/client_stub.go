package stubs

import (
	rest "k8s.io/client-go/rest"

	danmtypes "github.com/nokia/danm/pkg/crd/apis/danm/v1"
	client "github.com/nokia/danm/pkg/crd/client/clientset/versioned/typed/danm/v1"
)

type ClientStub struct {
	testNets []danmtypes.DanmNet
	testEps  []danmtypes.DanmEp
}

func (client *ClientStub) DanmNets(namespace string) client.DanmNetInterface {
	return newNetClientStub(client.testNets)
}

func (client *ClientStub) DanmEps(namespace string) client.DanmEpInterface {
	return newEpClientStub(client.testEps)
}

func (c *ClientStub) RESTClient() rest.Interface {
	return nil
}

func newClientStub(nets []danmtypes.DanmNet, eps []danmtypes.DanmEp) *ClientStub {
	return &ClientStub{
		testNets: nets,
		testEps:  eps,
	}
}
