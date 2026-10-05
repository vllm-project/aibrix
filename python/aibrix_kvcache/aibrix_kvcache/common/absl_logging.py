# Copyright 2024 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# 	http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Copyright 2017 The Abseil Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
"""Util functions from Abseil Python logging."""

import inspect
import io
import itertools
import logging
import os
import sys
import timeit
import traceback

_LOGGING_FILE_PREFIX = os.path.join("absl_logging")


def _fast_stack_trace():
    """A fast stack trace that gets us the minimal information we need.

    Returns:
      A tuple of tuples of (filename, line_number, last_instruction_offset).
    """
    cur_stack = inspect.currentframe()
    if cur_stack is None or cur_stack.f_back is None:
        return tuple()
    # We drop the first frame, which is this function itself.
    cur_stack = cur_stack.f_back
    call_stack = []
    while cur_stack.f_back:
        cur_stack = cur_stack.f_back
        call_stack.append(
            (
                cur_stack.f_code.co_filename,
                cur_stack.f_lineno,
                cur_stack.f_lasti,
            )
        )
    return tuple(call_stack)


# Counter to keep track of number of log entries per token.
_log_counter_per_token = {}


def _get_next_log_count_per_token(token):
    """Wrapper for _log_counter_per_token. Thread-safe.

    Args:
      token: The token for which to look up the count.

    Returns:
      The number of times this function has been called with
      *token* as an argument (starting at 0).
    """
    # Can't use a defaultdict because defaultdict isn't atomic, whereas
    # setdefault is.
    return next(_log_counter_per_token.setdefault(token, itertools.count()))


def log_every_n(logger, level, msg, n, *args, use_call_stack=True):
    """Logs ``msg % args`` at level 'level' once per 'n' times.

    Logs the 1st call, (N+1)st call, (2N+1)st call,  etc.
    Not threadsafe.

    Args:
      level: int, the absl logging level at which to log.
      msg: str, the message to be logged.
      n: int, the number of times this should be called before it is logged.
      *args: The args to be substituted into the msg.
      use_call_stack: bool, whether to include the call stack when counting the
        number of times the message is logged.
    """
    caller_info = logger.findCaller()
    if use_call_stack:
        # To reduce storage costs, we hash the call stack.
        caller_info = (*caller_info[0:3], hash(_fast_stack_trace()))
    count = _get_next_log_count_per_token(caller_info)
    log_if(logger, level, msg, not (count % n), *args)


# Keeps track of the last log time of the given token.
# Note: must be a dict since set/get is atomic in CPython.
# Note: entries are never released as their number is expected to be low.
_log_timer_per_token = {}


def _seconds_have_elapsed(token, num_seconds):
    """Tests if 'num_seconds' have passed since 'token' was requested.

    Not strictly thread-safe - may log with the wrong frequency if called
    concurrently from multiple threads. Accuracy depends on resolution of
    'timeit.default_timer()'.

    Always returns True on the first call for a given 'token'.

    Args:
      token: The token for which to look up the count.
      num_seconds: The number of seconds to test for.

    Returns:
      Whether it has been >= 'num_seconds' since 'token' was last requested.
    """
    now = timeit.default_timer()
    then = _log_timer_per_token.get(token)
    if then is None or (now - then) >= num_seconds:
        _log_timer_per_token[token] = now
        return True
    else:
        return False


def log_every_n_seconds(
    logger, level, msg, n_seconds, *args, use_call_stack=True
):
    """Logs ``msg % args`` at level ``level`` iff ``n_seconds`` elapsed
    since last call.

    Logs the first call, logs subsequent calls if 'n' seconds have elapsed
    since the last logging call from the same call site (file + line). Not
    thread-safe.

    Args:
      level: int, the absl logging level at which to log.
      msg: str, the message to be logged.
      n_seconds: float or int, seconds which should elapse before logging again.
      *args: The args to be substituted into the msg.
      use_call_stack: bool, whether to include the call stack when counting the
        number of times the message is logged.
    """
    caller_info = logger.findCaller()
    if use_call_stack:
        # To reduce storage costs, we hash the call stack.
        caller_info = (*caller_info[0:3], hash(_fast_stack_trace()))
    should_log = _seconds_have_elapsed(caller_info, n_seconds)
    log_if(logger, level, msg, should_log, *args)


def log_first_n(logger, level, msg, n, *args, use_call_stack=True):
    """Logs ``msg % args`` at level ``level`` only first ``n`` times.

    Not threadsafe.

    Args:
      level: int, the absl logging level at which to log.
      msg: str, the message to be logged.
      n: int, the maximal number of times the message is logged.
      *args: The args to be substituted into the msg.
      use_call_stack: bool, whether to include the call stack when counting the
        number of times the message is logged.
    """
    caller_info = logger.findCaller()
    if use_call_stack:
        # To reduce storage costs, we hash the call stack.
        caller_info = (*caller_info[0:3], hash(_fast_stack_trace()))
    count = _get_next_log_count_per_token(caller_info)
    log_if(logger, level, msg, count < n, *args)


def log_if(logger, level, msg, condition, *args):
    """Logs ``msg % args`` at level ``level`` only if condition is fulfilled."""
    if condition:
        logger.log(level, msg, *args)


class ABSLLogger(logging.getLoggerClass()):
    def findCaller(self, stack_info=False, stacklevel=1):
        """Finds the frame of the calling method on the stack.

        This method skips any methods from this file.

        Args:
          stack_info: bool, when True, include the stack trace as a fourth
            item returned.  On Python 3 there are always four items returned
            - the fourth will be None when this is False.  On Python 2 the
            stdlib base class API only returns three items.  We do the same
            when this new parameter is unspecified or False for compatibility.
          stacklevel: int, if greater than 1, that number of frames will be
            skipped.

        Returns:
          (filename, lineno, methodname[, sinfo]) of the calling method.
        """
        # Use sys._getframe(3) instead of logging.currentframe(), it's slightly
        # faster because there is one less frame to traverse.
        frame = sys._getframe(3)  # pylint: disable=protected-access
        frame_to_return = None

        while frame:
            code = frame.f_code
            if _LOGGING_FILE_PREFIX not in code.co_filename:
                frame_to_return = frame
                stacklevel -= 1
                if stacklevel <= 0:
                    break
            frame = frame.f_back

        if frame_to_return is not None:
            sinfo = None
            if stack_info:
                out = io.StringIO()
                out.write("Stack (most recent call last):\n")
                traceback.print_stack(frame, file=out)
                sinfo = out.getvalue().rstrip("\n")
            return (
                frame_to_return.f_code.co_filename,
                frame_to_return.f_lineno,
                frame_to_return.f_code.co_name,
                sinfo,
            )

        return None


def getLogger(name):
    """Return a logger with the specified name, creating it if necessary."""
    original_logger_class = logging.getLoggerClass()
    logging.setLoggerClass(ABSLLogger)
    absl_logger = logging.getLogger(name)
    logging.setLoggerClass(original_logger_class)
    return absl_logger
