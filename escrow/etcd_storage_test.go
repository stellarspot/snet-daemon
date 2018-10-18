package escrow

import (
	"bytes"
	"math/big"
	"net"
	"testing"
	"text/template"
	"time"

	"github.com/singnet/snet-daemon/config"
	"github.com/singnet/snet-daemon/etcddb"
	"github.com/singnet/snet-daemon/handler"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
)

var etcdPaymentHandler escrowPaymentHandler
var etcdStorageMock etcdStorageMockType

type etcdStorageMockType struct {
	*EtcdStorage
	keys []*PaymentChannelKey
}

func (storageMock *etcdStorageMockType) Put(key *PaymentChannelKey, state *PaymentChannelData) (err error) {
	storageMock.keys = append(storageMock.keys, key)
	return storageMock.EtcdStorage.Put(key, state)
}

func (storageMock *etcdStorageMockType) CompareAndSwap(
	key *PaymentChannelKey,
	prevState *PaymentChannelData,
	newState *PaymentChannelData,
) (ok bool, err error) {
	storageMock.keys = append(storageMock.keys, key)
	return storageMock.EtcdStorage.CompareAndSwap(key, prevState, newState)
}

func (storageMock *etcdStorageMockType) Clear() {
	for _, key := range storageMock.keys {
		bytes, _ := serialize(key)
		storageMock.client.Delete(bytes)
	}
	storageMock.keys = nil
}

type EtcdStorageTemplateType struct {
	ClientPort int
	PeerPort   int
}

func initEtcdStorage() (close func(), err error) {

	confJSON, err := getEtcdJSONConf()
	if err != nil {
		return
	}

	vip := viper.New()
	err = config.ReadConfigFromJsonString(vip, confJSON)
	if err != nil {
		return
	}

	server, err := etcddb.InitEtcdServer(vip)
	if err != nil {
		return
	}

	storage, err := NewEtcdStorage(vip)

	if err != nil {
		return
	}

	etcdStorageMock = etcdStorageMockType{EtcdStorage: storage}

	etcdPaymentHandler = escrowPaymentHandler{
		escrowContractAddress: testEscrowContractAddress,
		storage:               &etcdStorageMock,
		incomeValidator:       &incomeValidatorMock,
	}

	return func() {
		server.Close()
		storage.Close()
	}, nil
}

func getEtcdJSONConf() (json string, err error) {
	const confJSONTemplate = `
	{
		"PAYMENT_CHANNEL_STORAGE_CLIENT": {
			"CONNECTION_TIMEOUT": 5000,
			"REQUEST_TIMEOUT": 3000,
			"ENDPOINTS": ["http://127.0.0.1:{{.ClientPort}}"]
		},
		"PAYMENT_CHANNEL_STORAGE_SERVER": {
			"ID": "storage-1",
			"HOST" : "127.0.0.1",
			"CLIENT_PORT": {{.ClientPort}},
			"PEER_PORT": {{.PeerPort}},
			"TOKEN": "unique-token",
			"CLUSTER": "storage-1=http://127.0.0.1:{{.PeerPort}}",
			"ENABLED": true
		}
	}`

	tmpl, err := template.New("etcd").Parse(confJSONTemplate)
	if err != nil {
		return
	}

	clientPort, err := getFreePort()
	if err != nil {
		return
	}

	peerPort, err := getFreePort()
	if err != nil {
		return
	}

	data := EtcdStorageTemplateType{
		ClientPort: clientPort,
		PeerPort:   peerPort,
	}

	var buff bytes.Buffer
	err = tmpl.Execute(&buff, data)
	if err != nil {
		return
	}

	json = buff.String()

	log.WithFields(log.Fields{
		"client port": clientPort,
		"peer   port": peerPort,
	}).Info()

	log.Info("etcd config", json)

	return
}

func getFreePort() (port int, err error) {

	listener, err := net.Listen("tcp", ":0")

	if err != nil {
		return
	}

	defer listener.Close()

	port = listener.Addr().(*net.TCPAddr).Port
	return
}

func getTestEtcdContext(data *testPaymentData) *handler.GrpcStreamContext {
	etcdStorageMock.Put(
		newPaymentChannelKey(data.ChannelID, data.ChannelNonce),
		&PaymentChannelData{
			State:            data.State,
			Sender:           testPublicKey,
			FullAmount:       big.NewInt(data.FullAmount),
			Expiration:       data.Expiration,
			AuthorizedAmount: big.NewInt(data.PrevAmount),
			Signature:        nil,
		},
	)
	md := getEscrowMetadata(data.ChannelID, data.ChannelNonce, data.NewAmount)
	return &handler.GrpcStreamContext{
		MD: md,
	}
}

func clearTestEtcdContext() {
	etcdStorageMock.Clear()
}

func TestEtcdGetPayment(t *testing.T) {

	close, e := initEtcdStorage()
	if e != nil {
		t.Errorf("error during etcd storage initialization: %v", e)
	}
	defer close()

	data := &testPaymentData{
		ChannelID:    42,
		ChannelNonce: 3,
		Expiration:   time.Now().Add(time.Hour),
		FullAmount:   12345,
		NewAmount:    12345,
		PrevAmount:   12300,
		State:        Open,
	}
	context := getTestEtcdContext(data)
	defer clearTestEtcdContext()

	payment, err := etcdPaymentHandler.Payment(context)

	assert.Nil(t, err)
	expected := getTestPayment(data)
	actual := payment.(*escrowPaymentType)
	assert.Equal(t, toJSON(expected.grpcContext), toJSON(actual.grpcContext))
	assert.Equal(t, toJSON(expected.channelKey), toJSON(actual.channelKey))
	assert.Equal(t, expected.amount, actual.amount)
	assert.Equal(t, expected.signature, actual.signature)
	assert.Equal(t, toJSON(expected.channel), toJSON(actual.channel))
}