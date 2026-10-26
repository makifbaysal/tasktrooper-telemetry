package trooperfranz

import "go.opentelemetry.io/otel/attribute"

func attrKey(s string) attribute.Key { return attribute.Key(s) }
