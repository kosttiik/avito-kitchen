INSERT INTO establishments (id, name, kind, description, is_active)
VALUES (
    '11111111-1111-4111-8111-111111111111',
    'Example Kitchen',
    'restaurant',
    'Demo establishment for the Avito Kitchen integration flow',
    true
)
ON CONFLICT (id) DO UPDATE
SET name = EXCLUDED.name,
    kind = EXCLUDED.kind,
    description = EXCLUDED.description,
    is_active = EXCLUDED.is_active,
    updated_at = now();

INSERT INTO establishment_integrations (
    establishment_id,
    external_id,
    api_key_hash,
    is_active
)
VALUES (
    '11111111-1111-4111-8111-111111111111',
    'example-kitchen',
    decode('4b9a17ef45b17609408f199a4ea68171df87ca2519a300ff72e272e61db825c8', 'hex'),
    true
)
ON CONFLICT (establishment_id) DO UPDATE
SET external_id = EXCLUDED.external_id,
    api_key_hash = EXCLUDED.api_key_hash,
    is_active = EXCLUDED.is_active,
    updated_at = now();
