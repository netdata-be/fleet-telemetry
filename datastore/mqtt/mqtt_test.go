package mqtt_test

import (
	"context"
	"encoding/json"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/sirupsen/logrus/hooks/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/teslamotors/fleet-telemetry/protos"
	"github.com/teslamotors/fleet-telemetry/server/airbrake"

	"github.com/teslamotors/fleet-telemetry/datastore/mqtt"
	logrus "github.com/teslamotors/fleet-telemetry/logger"
	"github.com/teslamotors/fleet-telemetry/metrics"
	"github.com/teslamotors/fleet-telemetry/telemetry"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type MockMQTTClient struct {
	ConnectFunc           func() pahomqtt.Token
	PublishFunc           func(topic string, qos byte, retained bool, payload interface{}) pahomqtt.Token
	DisconnectFunc        func(quiesce uint)
	IsConnectedFunc       func() bool
	IsConnectionOpenFunc  func() bool
	SubscribeFunc         func(topic string, qos byte, callback pahomqtt.MessageHandler) pahomqtt.Token
	SubscribeMultipleFunc func(filters map[string]byte, callback pahomqtt.MessageHandler) pahomqtt.Token
	UnsubscribeFunc       func(topics ...string) pahomqtt.Token
	AddRouteFunc          func(topic string, callback pahomqtt.MessageHandler)
	OptionsReaderFunc     func() pahomqtt.ClientOptionsReader
}

func (m *MockMQTTClient) Connect() pahomqtt.Token {
	return m.ConnectFunc()
}

func (m *MockMQTTClient) Publish(topic string, qos byte, retained bool, payload interface{}) pahomqtt.Token {
	return m.PublishFunc(topic, qos, retained, payload)
}

func (m *MockMQTTClient) Disconnect(quiesce uint) {
	m.DisconnectFunc(quiesce)
}

func (m *MockMQTTClient) IsConnected() bool {
	return m.IsConnectedFunc()
}

func (m *MockMQTTClient) IsConnectionOpen() bool {
	return m.IsConnectionOpenFunc()
}

func (m *MockMQTTClient) Subscribe(topic string, qos byte, callback pahomqtt.MessageHandler) pahomqtt.Token {
	return m.SubscribeFunc(topic, qos, callback)
}

func (m *MockMQTTClient) SubscribeMultiple(filters map[string]byte, callback pahomqtt.MessageHandler) pahomqtt.Token {
	return m.SubscribeMultipleFunc(filters, callback)
}

func (m *MockMQTTClient) Unsubscribe(topics ...string) pahomqtt.Token {
	return m.UnsubscribeFunc(topics...)
}

func (m *MockMQTTClient) AddRoute(topic string, callback pahomqtt.MessageHandler) {
	m.AddRouteFunc(topic, callback)
}

func (m *MockMQTTClient) OptionsReader() pahomqtt.ClientOptionsReader {
	return m.OptionsReaderFunc()
}

type MockToken struct {
	WaitFunc        func() bool
	WaitTimeoutFunc func(time.Duration) bool
	DoneFunc        func() <-chan struct{}
	ErrorFunc       func() error
}

func (m *MockToken) Wait() bool {
	return m.WaitFunc()
}

func (m *MockToken) WaitTimeout(d time.Duration) bool {
	return m.WaitTimeoutFunc(d)
}

func (m *MockToken) Done() <-chan struct{} {
	return m.DoneFunc()
}

func (m *MockToken) Error() error {
	return m.ErrorFunc()
}

var publishedTopics = make(map[string][]byte)

func resetPublishedTopics() {
	publishedTopics = make(map[string][]byte)
}

func mockPahoNewClient(o *pahomqtt.ClientOptions) pahomqtt.Client {
	return &MockMQTTClient{

		ConnectFunc: func() pahomqtt.Token {
			return &MockToken{
				WaitFunc:  func() bool { return true },
				ErrorFunc: func() error { return nil },
			}
		},
		IsConnectedFunc: func() bool {
			return true
		},
		PublishFunc: func(topic string, qos byte, retained bool, payload interface{}) pahomqtt.Token {
			publishedTopics[topic] = payload.([]byte)
			return &MockToken{
				WaitTimeoutFunc: func(d time.Duration) bool { return true },
				WaitFunc:        func() bool { return true },
				ErrorFunc:       func() error { return nil },
			}
		},
	}
}

var _ = Describe("MQTTProducer", func() {
	var (
		mockLogger        *logrus.Logger
		mockCollector     metrics.MetricCollector
		mockConfig        *mqtt.Config
		mockAirbrake      *airbrake.AirbrakeHandler
		originalNewClient func(*pahomqtt.ClientOptions) pahomqtt.Client
		loggerHook        *test.Hook
	)

	BeforeEach(func() {
		resetPublishedTopics()
		originalNewClient = mqtt.PahoNewClient
		mqtt.PahoNewClient = mockPahoNewClient

		mockLogger, loggerHook = logrus.NoOpLogger()
		mockCollector = metrics.NewCollector(nil, mockLogger)
		mockAirbrake = airbrake.NewAirbrakeHandler(nil)
		mockConfig = &mqtt.Config{
			Broker:    "tcp://localhost:1883",
			ClientID:  "test-client",
			Username:  "testuser",
			Password:  "testpass",
			TopicBase: "test/topic",
			QoS:       1,
			Retained:  false,
		}
	})

	AfterEach(func() {
		mqtt.PahoNewClient = originalNewClient
	})

	Describe("Produce", func() {
		It("should publish MQTT messages for each field in the payload", func() {
			producer, err := mqtt.NewProducer(
				context.Background(),
				mockConfig,
				mockCollector,
				"test_namespace",
				mockAirbrake,
				nil,
				nil,
				mockLogger,
			)
			Expect(err).NotTo(HaveOccurred())

			payload := &protos.Payload{
				Vin: "TEST123",
				Data: []*protos.Datum{
					{
						Key: protos.Field_VehicleName,
						Value: &protos.Value{
							Value: &protos.Value_StringValue{StringValue: "My Tesla"},
						},
					},
					{
						Key: protos.Field_BatteryLevel,
						Value: &protos.Value{
							Value: &protos.Value_FloatValue{FloatValue: 75.5},
						},
					},
				},
				CreatedAt: timestamppb.Now(),
			}

			payloadBytes, err := proto.Marshal(payload)
			Expect(err).NotTo(HaveOccurred())

			record := &telemetry.Record{
				TxType:       "V",
				Vin:          "TEST123",
				PayloadBytes: payloadBytes,
			}

			producer.Produce(record)

			Expect(publishedTopics).To(HaveLen(2))

			vehicleNameTopic := "test/topic/TEST123/v/VehicleName"
			batteryLevelTopic := "test/topic/TEST123/v/BatteryLevel"

			vehicleNameValue := "{\"value\":\"My Tesla\"}"
			batterLevelValue := "{\"value\":75.5}"

			vehicleNameValue := "{\"value\":\"My Tesla\"}"
			batterLevelValue := "{\"value\":75.5}"

			Expect(publishedTopics).To(HaveKey(vehicleNameTopic))
			Expect(publishedTopics).To(HaveKey(batteryLevelTopic))
			Expect(publishedTopics[vehicleNameTopic]).To(Equal([]byte(vehicleNameValue)))
			Expect(publishedTopics[batteryLevelTopic]).To(Equal([]byte(batterLevelValue)))
<<<<<<< HEAD
		})

		It("should publish MQTT messages for vehicle alerts", func() {

			producer, err := mqtt.NewProducer(
				context.Background(),
				mockConfig,
				mockCollector,
				"test_namespace",
				mockAirbrake,
				nil,
				nil,
				mockLogger,
			)
			Expect(err).NotTo(HaveOccurred())

			alerts := &protos.VehicleAlerts{
				Vin: "TEST123",
				Alerts: []*protos.VehicleAlert{
					{
						Name:      "TestAlert1",
						StartedAt: timestamppb.Now(),
						EndedAt:   nil,
						Audiences: []protos.Audience{protos.Audience_Customer, protos.Audience_Service},
					},
					{
						Name:      "TestAlert2",
						StartedAt: timestamppb.Now(),
						EndedAt:   timestamppb.Now(),
						Audiences: []protos.Audience{protos.Audience_ServiceFix},
					},
				},
				CreatedAt: timestamppb.Now(),
			}

			alertsBytes, err := proto.Marshal(alerts)
			Expect(err).NotTo(HaveOccurred())

			record := &telemetry.Record{
				TxType:       "alerts",
				Vin:          "TEST123",
				PayloadBytes: alertsBytes,
			}

			producer.Produce(record)

			Expect(publishedTopics).To(HaveLen(4))

			alert1CurrentTopic := "test/topic/TEST123/alerts/TestAlert1/current"
			alert1HistoryTopic := "test/topic/TEST123/alerts/TestAlert1/history"
			alert2CurrentTopic := "test/topic/TEST123/alerts/TestAlert2/current"
			alert2HistoryTopic := "test/topic/TEST123/alerts/TestAlert2/history"

			Expect(publishedTopics).To(HaveKey(alert1CurrentTopic))
			Expect(publishedTopics).To(HaveKey(alert1HistoryTopic))
			Expect(publishedTopics).To(HaveKey(alert2CurrentTopic))
			Expect(publishedTopics).To(HaveKey(alert2HistoryTopic))

			var alert1Current, alert2Current map[string]interface{}
			var alert1History, alert2History []map[string]interface{}
			json.Unmarshal(publishedTopics[alert1CurrentTopic], &alert1Current)
			json.Unmarshal(publishedTopics[alert1HistoryTopic], &alert1History)
			json.Unmarshal(publishedTopics[alert2CurrentTopic], &alert2Current)
			json.Unmarshal(publishedTopics[alert2HistoryTopic], &alert2History)

			Expect(alert1Current).To(HaveKey("StartedAt"))
			Expect(alert1Current).NotTo(HaveKey("EndedAt"))
			Expect(alert1Current["Audiences"]).To(ConsistOf("Customer", "Service"))

			Expect(alert2Current).To(HaveKey("StartedAt"))
			Expect(alert2Current).To(HaveKey("EndedAt"))
			Expect(alert2Current["Audiences"]).To(ConsistOf("ServiceFix"))

			Expect(alert1History).To(BeAssignableToTypeOf([]map[string]interface{}{}))
			Expect(alert2History).To(BeAssignableToTypeOf([]map[string]interface{}{}))
		})

		It("should publish MQTT messages for vehicle errors", func() {
			producer, err := mqtt.NewProducer(
				context.Background(),
				mockConfig,
				mockCollector,
				"test_namespace",
				nil,
				nil,
				nil,
				mockLogger,
			)
			Expect(err).NotTo(HaveOccurred())

			vehicleErrors := &protos.VehicleErrors{
				Vin: "TEST123",
				Errors: []*protos.VehicleError{
					{
						Name:      "TestError1",
						Body:      "This is a test error",
						Tags:      map[string]string{"tag1": "value1", "tag2": "value2"},
						CreatedAt: timestamppb.Now(),
					},
					{
						Name:      "TestError2",
						Body:      "This is another test error",
						Tags:      map[string]string{"tagA": "valueA"},
						CreatedAt: timestamppb.Now(),
					},
				},
				CreatedAt: timestamppb.Now(),
			}

			errorsBytes, err := proto.Marshal(vehicleErrors)
			Expect(err).NotTo(HaveOccurred())

			record := &telemetry.Record{
				TxType:       "errors",
				Vin:          "TEST123",
				PayloadBytes: errorsBytes,
			}

			producer.Produce(record)

			Expect(publishedTopics).To(HaveLen(2))

			error1Topic := "test/topic/TEST123/errors/TestError1"
			error2Topic := "test/topic/TEST123/errors/TestError2"

			Expect(publishedTopics).To(HaveKey(error1Topic))
			Expect(publishedTopics).To(HaveKey(error2Topic))

			var error1, error2 map[string]interface{}
			json.Unmarshal(publishedTopics[error1Topic], &error1)
			json.Unmarshal(publishedTopics[error2Topic], &error2)

			Expect(error1).To(HaveKey("Body"))
			Expect(error1["Body"]).To(Equal("This is a test error"))
			Expect(error1["Tags"]).To(HaveKeyWithValue("tag1", "value1"))
			Expect(error1["Tags"]).To(HaveKeyWithValue("tag2", "value2"))
			Expect(error1).To(HaveKey("CreatedAt"))

			Expect(error2).To(HaveKey("Body"))
			Expect(error2["Body"]).To(Equal("This is another test error"))
			Expect(error2["Tags"]).To(HaveKeyWithValue("tagA", "valueA"))
			Expect(error2).To(HaveKey("CreatedAt"))
		})

		It("should handle timeouts when publishing MQTT messages", func() {
			// Mock a slow publish function that always times out
			mqtt.PahoNewClient = func(o *pahomqtt.ClientOptions) pahomqtt.Client {
				return &MockMQTTClient{
					ConnectFunc: func() pahomqtt.Token {
						return &MockToken{
							WaitFunc:  func() bool { return true },
							ErrorFunc: func() error { return nil },
						}
					},
					IsConnectedFunc: func() bool {
						return true
					},
					PublishFunc: func(topic string, qos byte, retained bool, payload interface{}) pahomqtt.Token {
						return &MockToken{
							WaitTimeoutFunc: func(d time.Duration) bool { return false },
							WaitFunc:        func() bool { return false },
							ErrorFunc:       func() error { return pahomqtt.TimedOut },
						}
					},
				}
			}

			producer, err := mqtt.NewProducer(
				context.Background(),
				mockConfig,
				mockCollector,
				"test_namespace",
				mockAirbrake,
				nil,
				nil,
				mockLogger,
			)
			Expect(err).NotTo(HaveOccurred())

			payload := &protos.Payload{
				Vin: "TEST123",
				Data: []*protos.Datum{
					{
						Key: protos.Field_VehicleName,
						Value: &protos.Value{
							Value: &protos.Value_StringValue{StringValue: "My Tesla"},
						},
					},
				},
				CreatedAt: timestamppb.Now(),
			}

			payloadBytes, err := proto.Marshal(payload)
			Expect(err).NotTo(HaveOccurred())

			record := &telemetry.Record{
				TxType:       "V",
				Vin:          "TEST123",
				PayloadBytes: payloadBytes,
			}
			producer.Produce(record)

			// Check that an error was logged
			Expect(loggerHook.LastEntry().Message).To(Equal("mqtt_publish_error"))

=======
>>>>>>> 293485f (unit test mqtt value)
		})
	})
})
