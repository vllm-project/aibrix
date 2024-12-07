import asyncio
import atexit
import heapq
import json
import sys
import threading
from typing import List

from vidur.config import SimulationConfig
from vidur.entities import Cluster, Request
from vidur.events import BaseEvent, RequestArrivalEvent
from vidur.logger import init_logger
from vidur.metrics import MetricsStore
from vidur.request_generator import RequestGeneratorRegistry
from vidur.scheduler import BaseGlobalScheduler, GlobalSchedulerRegistry
from vidur.types import EventType
from vidur.utils.random import set_seeds

logger = init_logger(__name__)


class Simulator:
    def __init__(self, config: SimulationConfig) -> None:
        self._config: SimulationConfig = config
        set_seeds(config.seed)

        self._done = None
        self._time = 0
        self._terminate = False
        self._time_limit = self._config.time_limit
        if not self._time_limit:
            self._time_limit = float("inf")

        self._event_queue = []

        self._event_trace = []
        self._event_chrome_trace = []

        self._cluster = Cluster(
            self._config.cluster_config,
            self._config.metrics_config,
            self._config.request_generator_config,
        )
        self._metric_store = MetricsStore(self._config)
        self._request_generator = RequestGeneratorRegistry.get(
            self._config.request_generator_config.get_type(),
            self._config.request_generator_config,
        )
        self._scheduler = GlobalSchedulerRegistry.get(
            self._config.cluster_config.global_scheduler_config.get_type(),
            self._config,
            self._cluster.replicas,
        )

        self._loop = None
        self._expect_next_tick = sys.float_info.max
        self._queue_buffer: List[Request] = []
        self._queue = None

        # self._init_event_queue()
        atexit.register(self._write_output)

    @property
    def scheduler(self) -> BaseGlobalScheduler:
        return self._scheduler

    @property
    def metric_store(self) -> MetricsStore:
        return self._metric_store

    def start(self):
        logger.info(
            f"Starting simulation with cluster: {self._cluster}, model: {self._config.cluster_config.replica_config.model_name}, seed: {self._config.seed}"
        )

        # Start the event loop
        self._loop = asyncio.new_event_loop()
        self._queue = asyncio.Queue()
        self._done = asyncio.Event()

        # Create and start a new thread to run the loop
        t = threading.Thread(target=self._run)
        t.start()

        asyncio.run_coroutine_threadsafe(self._serve(), self._loop)

        return t

    def _run(self):
        asyncio.set_event_loop(self._loop)
        self._loop.run_forever()

    def stop(self):
        asyncio.run_coroutine_threadsafe(self._wait_done(asyncio.all_tasks(loop=self._loop)), self._loop).result()
        self._loop.call_soon_threadsafe(self._loop.stop)

    async def _wait_done(self, pending):
        # Graceful shutdown (if needed)
        logger.info(f"pending:{len(pending)}")
        for task in pending:
            task.cancel()
        await asyncio.gather(*pending, return_exceptions=True)

    async def _serve(self):
        while True:
            # Enqueue arrived requests.
            if not self._queue.empty():
                request: Request = await self._queue.get()
                self._serve_request(request)
                self._queue.task_done()  # Signal that the task is complete
                continue

            # Drive events.
            while self._event_queue and not self._terminate:
                _, event = heapq.heappop(self._event_queue)
                self._set_time(event._time)
                new_events = event.handle_event(self._scheduler, self._metric_store)
                self._add_events(new_events)
                logger.debug("Executed event: %s", event)

                if self._config.metrics_config.write_json_trace:
                    self._event_trace.append(event.to_dict())

                if self._config.metrics_config.enable_chrome_trace:
                    chrome_trace = event.to_chrome_trace()
                    if chrome_trace:
                        self._event_chrome_trace.append(chrome_trace)

                if event.event_type == EventType.REQUEST_END and event._request.response != None:
                    event._request.response.set_result(event._time - event._request.arrived_at)

                # Pause at the next request predicts.
                if self._expect_next_tick > 0 and event._time >= self._expect_next_tick:
                    self._expect_next_tick = 0
                    break

            # Reset expecting next request.
            # if self._expect_next_tick == 0 and (not self._event_queue or self._terminate):
            #     return

            # Expecting next request
            request: Request = await self._queue.get()
            self._serve_request(request)
            self._queue.task_done()  # Signal that the task is complete

    def _serve_request(self, request: Request):
        # Update next expected request.
        if self._expect_next_tick == 0:
            self._expect_next_tick = request.arrived_next
        else:
            self._expect_next_tick = min(self._expect_next_tick, request.arrived_next)

        self._add_event(RequestArrivalEvent(request.arrived_at, request))

    def execute(self, request: Request) -> float:
        return asyncio.run_coroutine_threadsafe(self._execute(request), self._loop).result()

    async def _execute(self, request: Request) -> float:
        if self._queue is None:
            self._queue_buffer.append(request)
            return 0.0

        request.response = self._loop.create_future()
        await self._queue.put(request)

        result = await request.response
        return result

    def _write_output(self) -> None:
        logger.info("Writing output")

        self._metric_store.plot()
        logger.info("Metrics written")

        if self._config.metrics_config.write_json_trace:
            self._write_event_trace()
            self._scheduler.write_batching_history()
            logger.info("Json event trace written")

        if self._config.metrics_config.enable_chrome_trace:
            self._write_chrome_trace()
            logger.info("Chrome event trace written")

    def _add_event(self, event: BaseEvent, queue=None) -> None:
        if queue is None:
            queue = self._event_queue
        heapq.heappush(queue, (event._priority_number, event))

    def _add_events(self, events: List[BaseEvent], queue=None) -> None:
        for event in events:
            self._add_event(event, queue)

    def _init_event_queue(self) -> None:
        requests = self._request_generator.generate()

        for request in requests:
            self._add_event(RequestArrivalEvent(request.arrived_at, request))

    def _set_time(self, time: float) -> None:
        self._time = time
        if self._time > self._time_limit:
            logger.info(
                f"Time limit reached: {self._time_limit}s terminating the simulation."
            )
            self._terminate = True

    def _write_event_trace(self) -> None:
        trace_file = f"{self._config.metrics_config.output_dir}/event_trace.json"
        with open(trace_file, "w") as f:
            json.dump(self._event_trace, f)

    def _write_chrome_trace(self) -> None:
        trace_file = f"{self._config.metrics_config.output_dir}/chrome_trace.json"

        chrome_trace = {"traceEvents": self._event_chrome_trace}

        with open(trace_file, "w") as f:
            json.dump(chrome_trace, f)
