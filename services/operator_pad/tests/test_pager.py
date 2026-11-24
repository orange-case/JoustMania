"""Auto-paging, the return to Home and the knob's page steps (UC-016 A5, A6, BR-008, BR-009), injected clock."""

import pytest

from proto import menu_pb2
from services.operator_pad.display import DisplayModel
from services.operator_pad.pages import HOME, NET_DOWN, NET_OK, NET_UNKNOWN, PAGES, Pager
from services.operator_pad.tests.display_fakes import pad_config

AUTO, HOME_S = 5.0, 10.0


def lobby(**kw) -> menu_pb2.GetMenuStatusResponse:
    base = {"selection": "JoustFFA", "ready_count": 1, "connected_count": 2, "audio_volume_master": "normal"}
    base.update(kw)
    return menu_pb2.GetMenuStatusResponse(**base)


@pytest.mark.uc("UC-016", flow="A6", br=["BR-009"], steps=[1])
def test_pages_advance_while_idle_and_wrap():
    pager = Pager(0.0)
    assert not pager.tick(4.9, True, AUTO, HOME_S) and pager.page == HOME
    seen = []
    for k in range(1, len(PAGES) + 1):
        assert pager.tick(5.0 * k, True, AUTO, HOME_S)
        seen.append(pager.page)
    assert seen == [1, 2, 3, 4, 5, 6, 0]


@pytest.mark.uc("UC-016", flow="A6", br=["BR-009"], steps=[1])
def test_no_auto_paging_while_not_idle_or_off():
    pager = Pager(0.0)
    assert not pager.tick(60.0, False, AUTO, HOME_S) and pager.page == HOME
    assert not pager.tick(60.0, True, 0.0, HOME_S)  # `off`
    assert pager.next_wake(False, AUTO, HOME_S) is None and pager.next_wake(True, 0.0, HOME_S) is None


@pytest.mark.uc("UC-016", flow="A6", br=["BR-009"], steps=[2, 3])
def test_scroll_stops_cycling_and_home_returns_once_after_the_last_input():
    pager = Pager(0.0)
    pager.input(1.0)
    pager.step(2, 1.0)
    assert pager.page == 2 and not pager.cycling
    assert not pager.tick(9.0, True, AUTO, HOME_S) and pager.page == 2  # stopped: no advance
    assert pager.next_wake(True, AUTO, HOME_S) == 11.0
    assert pager.tick(11.0, True, AUTO, HOME_S) and pager.page == HOME and pager.cycling
    assert pager.tick(16.0, True, AUTO, HOME_S) and pager.page == 1  # cycling resumed
    assert not pager.tick(30.0, False, AUTO, HOME_S) and pager.page == 1  # once: no second return


@pytest.mark.uc("UC-016", flow="A6", br=["BR-009"], steps=[3])
def test_home_return_with_autopage_off_shows_home_and_stays():
    pager = Pager(0.0)
    pager.input(0.0)
    pager.step(-1, 0.0)
    assert pager.page == len(PAGES) - 1
    assert pager.tick(10.0, True, 0.0, HOME_S) and pager.page == HOME
    assert not pager.tick(100.0, True, 0.0, HOME_S) and pager.page == HOME


@pytest.mark.uc("UC-016", flow="A6", br=["BR-009"], steps=[3])
def test_home_return_waits_for_keys_up():
    """A key held past display_home_ms never changes the page under it: the return comes when idle again."""
    pager = Pager(0.0)
    pager.input(0.0)
    pager.step(2, 0.0)
    assert not pager.tick(10.0, False, AUTO, HOME_S) and pager.page == 2
    assert not pager.tick(25.0, False, AUTO, HOME_S) and pager.page == 2
    assert pager.next_wake(False, AUTO, HOME_S) is None
    assert pager.next_wake(True, AUTO, HOME_S) == 10.0  # due: applies at the next idle tick
    assert pager.tick(25.5, True, AUTO, HOME_S) and pager.page == HOME and pager.cycling


@pytest.mark.uc("UC-016", flow="A6", br=["BR-009"])
def test_no_home_return_without_input_since_link_up_and_none_at_home_while_cycling():
    pager = Pager(0.0)
    pager.tick(5.0, True, AUTO, HOME_S)
    assert pager.page == 1
    assert pager.next_wake(True, 0.0, HOME_S) is None  # no input since the link came up: no return
    pager.reset(20.0)
    pager.input(20.0)
    assert not pager.tick(30.0, False, AUTO, HOME_S)  # at HOME and cycling: nothing to return from


class Model:
    def __init__(self, version: int = 3, autopage_ms: int = 5000, home_ms: int = 10000) -> None:
        self.now = 100.0
        self.config = pad_config(autopage_ms=autopage_ms, home_ms=home_ms)
        self.model = DisplayModel(self.config, version="0.14.0", clock=lambda: self.now)
        self.model.set_protocol(version)
        self.model.acknowledged()
        self.model.note_status(lobby(), self.model.read_token())

    def title(self) -> str:
        return self.model.current(self.now).lines[0][0].rstrip()


@pytest.mark.uc("UC-016", flow="A6", br=["BR-009", "BR-006"])
def test_model_link_up_shows_home_and_a_status_change_moves_no_page():
    m = Model()
    assert m.title().startswith("HOME")
    m.now += 4.0
    m.model.note_status(lobby(ready_count=2))
    m.now += 1.0
    assert m.title().startswith("GAME")  # 5 s after link-up, not 5 s after the status change
    assert m.model.next_change(m.now) == pytest.approx(m.now + 5.0)


@pytest.mark.uc("UC-016", flow="A6", br=["BR-009"], steps=[1])
def test_model_pauses_auto_paging_while_a_key_is_down_and_restarts_the_period_at_release():
    m = Model()
    m.now += 1.0
    m.model.note_input(True)
    m.now += 20.0
    assert m.title().startswith("HOME")
    assert m.model.next_change(m.now) is None  # held: no advance; the home return found HOME cycling
    m.model.note_input(False)
    m.now += 4.0
    assert m.title().startswith("HOME")
    m.now += 1.0
    assert m.title().startswith("GAME")


@pytest.mark.uc("UC-016", flow="A5", br=["BR-008", "BR-007"], steps=[2])
def test_model_page_step_ends_a_result_screen_and_counts_from_the_page_under_it():
    m = Model()
    m.model.result("SENSITIVITY", "FAST")
    assert m.title() == "SENSITIVITY"
    m.model.page_step(-1)
    assert m.title().startswith("SYSTEM") and m.title().endswith("7/7")
    m.model.page_step(1)
    assert m.title().startswith("HOME")


@pytest.mark.uc("UC-016", flow="A5", br=["BR-008"], steps=[2])
def test_model_volume_limit_after_a_page_step_keeps_the_page():
    """m7: a commit that lands at the limit after a page step does not put the volume screen back."""
    m = Model()
    m.model.volume_turn(1)
    ticket = m.model.volume_commit_start(("steps", 1))
    m.model.page_step(1)
    m.model.volume_commit_end(ticket, "at_limit", "")
    assert m.title().startswith("GAME")


@pytest.mark.uc("UC-016", flow="main", br=["BR-007"], steps=[6])
def test_model_volume_limit_without_a_page_step_shows_the_volume_screen():
    m = Model()
    m.model.volume_turn(1)
    ticket = m.model.volume_commit_start(("steps", 1))
    m.model.volume_commit_end(ticket, "at_limit", "")
    assert m.title().startswith("VOLUME") and m.title().endswith("MAXIMUM")


@pytest.mark.uc("UC-016", flow="main", br=["BR-007"], steps=[6])
def test_model_volume_marker_is_the_level_when_the_turn_began():
    m = Model()
    m.model.volume_turn(1)
    m.model.volume_turn(2)
    screen = m.model.current(m.now)
    assert screen.bar[2:4] == (5, 2)  # NEW `max` filled, marker on `normal`


@pytest.mark.uc("UC-016", flow="A6", br=["BR-009"], steps=[1])
def test_model_page_under_a_long_result_shows_for_a_full_period_after_it():
    """A late result (pending 6 s + result) outlasts the period: the page does not advance as it comes back."""
    m = Model()
    m.model.note_input(True)
    m.model.note_input(False)
    m.model.pending("SENSITIVITY")
    m.now += 6.0
    m.model.result("SENSITIVITY", "FAST")
    m.now += m.config.display_result_ms() / 1000
    assert m.title().startswith("HOME")
    m.now += 4.9
    assert m.title().startswith("HOME")
    m.now += 0.1
    assert m.title().startswith("GAME")


@pytest.mark.uc("UC-016", flow="A6", br=["BR-009"])
def test_model_next_change_is_never_in_the_past():
    """The render loop never spins: a due deadline is always ahead of now."""
    m = Model()
    m.now += 7.0  # the advance at +5 s is overdue until current() runs
    assert m.model.next_change(m.now) > m.now
    m.model.current(m.now)
    assert m.model.next_change(m.now) == pytest.approx(m.now + 5.0)


@pytest.mark.uc("UC-016", flow="main", br=["BR-010"])
def test_model_internet_state_survives_a_short_flap_and_resets_after_a_long_one():
    m = Model()
    m.model.note_internet(NET_OK)
    m.model.set_protocol(None)
    m.now += 5.0
    m.model.set_protocol(3)
    assert m.model.internet == NET_OK and m.model.internet_wake.is_set()
    m.model.note_internet(NET_DOWN)
    m.model.set_protocol(None)
    m.now += 31.0
    m.model.set_protocol(3)
    assert m.model.internet == NET_UNKNOWN


@pytest.mark.uc("UC-016", flow="main", br=["BR-002"])
def test_model_version_above_4_is_used_as_4_and_a_refused_wheel_or_bar_lowers_the_layout():
    m = Model(version=9)
    assert m.model.protocol == 4
    m.model.layout_refused(0x04)
    assert m.model._layout == 3
    m.model.layout_refused(0x03)
    assert m.model._layout == 2
    m.model.set_protocol(4)  # a new link starts from the hello again
    assert m.model._layout == 4


@pytest.mark.uc("UC-016", flow="A5", br=["BR-008"])
def test_pages_active_needs_the_link_up_and_protocol_2():
    m = Model(version=2)
    assert m.model.pages_active
    m.model.set_protocol(1)
    assert not m.model.pages_active
    m.model.set_protocol(3)
    m.model.link_down()
    assert not m.model.pages_active
