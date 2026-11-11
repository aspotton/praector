from praector.util import dedupe_key, sql_literal


def test_dedupe_normalizes_whitespace_and_case():
    assert dedupe_key("research", "Find   OAuth") == dedupe_key("research", " find oauth ")


def test_sql_literal_escapes_quotes():
    assert sql_literal("it's ok") == "'it''s ok'"
