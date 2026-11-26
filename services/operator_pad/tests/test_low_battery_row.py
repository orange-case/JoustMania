"""The `START` page's read-only `LOW` row (UC-016 BR-006; UC-002 A8, BR-009)."""

import pytest

from proto import menu_pb2
from services.operator_pad.page_keys import ROWS
from services.operator_pad.pages import PAGES, PageContext, page_rows

START = PAGES.index("START")


@pytest.mark.uc("UC-016", br=["BR-006"])
@pytest.mark.uc("UC-002", flow="A8", br=["BR-009"])
@pytest.mark.parametrize("version", [2, 3, 4])
@pytest.mark.parametrize(
    ("status", "expected"),
    [
        (menu_pb2.GetMenuStatusResponse(low_battery_count=2), "2"),
        (menu_pb2.GetMenuStatusResponse(low_battery_count=0), "0"),
        (menu_pb2.GetMenuStatusResponse(), "?"),  # a menu older than field 30
        (None, "?"),
    ],
)
def test_start_row_2_shows_the_low_battery_count(version, status, expected):
    rows = page_rows(START, status, PageContext(), version)
    assert rows[1] == ("LOW", expected)


@pytest.mark.uc("UC-016", br=["BR-006"])
def test_the_low_row_has_no_edit_key():
    assert ROWS["START"][1] is None
