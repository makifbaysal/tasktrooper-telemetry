// Package troopermongo instruments the official MongoDB Go driver v2 (go.mongodb.org/mongo-driver/v2)
// with an event.CommandMonitor that creates OTel client spans.
//
// The upstream go.opentelemetry.io/contrib otelmongo package only supports driver v1 in a released
// version, so this package implements the (small) monitor itself, following the OTel database
// semantic conventions: db.system(.name)=mongodb, db.namespace, db.collection.name,
// db.operation.name, server.address/port. Command text is NOT recorded by default;
// WithCommandText records it with every value replaced by "?".
//
//	opts := options.Client().ApplyURI(uri).SetMonitor(troopermongo.NewMonitor())
//	client, err := mongo.Connect(opts)
package troopermongo
