"""Daily-note calendar: date listing + validation."""


def test_daily_dates_lists_existing(client):
    client.get("/api/daily?date=2026-03-14")   # creates the daily note
    client.get("/api/daily?date=2026-03-15")
    dates = client.get("/api/daily/dates").json()
    assert "2026-03-14" in dates and "2026-03-15" in dates


def test_daily_rejects_bad_dates(client):
    assert client.get("/api/daily?date=not-a-date").status_code == 400
    assert client.get("/api/daily?date=../../etc/passwd").status_code == 400
    assert client.get("/api/daily?date=2026-13-99").status_code == 200  # well-formed, allowed
