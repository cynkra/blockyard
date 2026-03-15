package config

import (
	"context"
	"fmt"
	"reflect"
)

// ResolveSecrets walks all Secret and *Secret fields in cfg and
// resolves any vault references (values starting with "vault:").
// Uses the same struct-walking pattern as applyEnvToStruct.
func ResolveSecrets(ctx context.Context, cfg *Config, resolver SecretResolver) error {
	return resolveStruct(ctx, reflect.ValueOf(cfg).Elem(), "config", resolver)
}

func resolveStruct(ctx context.Context, v reflect.Value, path string, resolver SecretResolver) error {
	t := v.Type()
	for i := range t.NumField() {
		field := t.Field(i)
		tag := field.Tag.Get("toml")
		if tag == "" || tag == "-" {
			continue
		}
		fieldPath := path + "." + tag
		fv := v.Field(i)

		// Pointer-to-struct: dereference if non-nil and recurse.
		if field.Type.Kind() == reflect.Ptr && field.Type.Elem().Kind() == reflect.Struct {
			if field.Type == secretPtrType {
				if fv.IsNil() {
					continue
				}
				s := fv.Interface().(*Secret)
				if err := s.Resolve(ctx, resolver); err != nil {
					return fmt.Errorf("%s: %w", fieldPath, err)
				}
				continue
			}
			if fv.IsNil() {
				continue
			}
			if err := resolveStruct(ctx, fv.Elem(), fieldPath, resolver); err != nil {
				return err
			}
			continue
		}

		// Secret (non-pointer).
		if field.Type == secretType {
			s := fv.Addr().Interface().(*Secret)
			if err := s.Resolve(ctx, resolver); err != nil {
				return fmt.Errorf("%s: %w", fieldPath, err)
			}
			continue
		}

		// Recurse into nested config sections (but not Duration/Secret).
		if field.Type.Kind() == reflect.Struct && field.Type != durationType && field.Type != secretType {
			if err := resolveStruct(ctx, fv, fieldPath, resolver); err != nil {
				return err
			}
			continue
		}
	}
	return nil
}
