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

import logging
import os
import threading
from datetime import datetime
from typing import Any, Callable, List, Optional, Tuple

import dash
import matplotlib.pyplot as plt
import numpy as np
import pandas as pd
import plotly.graph_objs as go
from dash import dcc, html
from dash.dependencies import Input, Output
from starlette.middleware.wsgi import WSGIMiddleware
from starlette.routing import Mount

from .load_reader import DatasetLoadReader, LoadReader
from .monitor import ModelMonitor
from .profile_reader import FileProfileReader, ProfileReader, RedisProfileReader

canvas_size = 1000
scale = 16
interval = 1000  # in milliseconds
reader_interval = 10  # in seconds
interval_scaling = interval / 1000 / reader_interval

# window = 4000
# # Load dataset reader
# directory = os.path.dirname(os.path.abspath(__file__))
# reader: LoadReader = DatasetLoadReader(directory + '/data/sharegpt.csv', n_batch=100)
# span = window / reader.n_batch
# # Define clusterer
# clusterers: List[Clusterer] = [MovingDBSCANClusterer(0.8, 100, 4, window), DBSCANClusterer(0.5, 10)]
# Simulated data source for display
# data = DataBuffer(window)
# lvl2data = DataBuffer(window)

colors = [
    "red",
    "green",
    "pink",
    "blue",
    "navy",
    "orange",
    "purple",
    "cyan",
    "magenta",
    "yellow",
    "black",
    "gray",
    "brown",
    "olive",
    "teal",
    "maroon",
]


def default_datasource(_: str) -> Optional[ModelMonitor]:
    return get_debug_model_montior(None)


debug_monitor = None
figure = type("", (), {})()  # create a empty object
figure.__dict__ = {
    "debug_run": True,
    "debug_driver": None,
    "datasource": default_datasource,
    "last": dash.no_update,
    "lock": threading.Lock(),
}

logger = logging.getLogger("aibrix.gpuoptimizer.loadmonitor.visualizer")


def get_debug_model_montior(
    path: Optional[str],
    scale: float = 1.0,
    profile: Optional[str] = None,
    redisprofile: Optional[str] = None,
) -> Optional[ModelMonitor]:
    global debug_monitor

    if debug_monitor is None:
        if path is None:
            directory = os.path.dirname(os.path.abspath(__file__))
            path = directory + "/data/sharegpt.csv"
        loadReader: LoadReader = DatasetLoadReader(
            path, rps=10, scale=scale, interval=reader_interval
        )

        profile_reader: Optional[ProfileReader] = None
        if profile is not None:
            profile_reader = FileProfileReader(profile)
        elif redisprofile is not None:
            profile_reader = RedisProfileReader(
                *parse_redis_connection_str(redisprofile)
            )

        debug_monitor = ModelMonitor(
            "sharegpt", "0", loadReader, profile_reader=profile_reader, debug=True
        )

    return debug_monitor


def parse_redis_connection_str(connection_str: str) -> Tuple[Any, str]:
    from urllib.parse import parse_qs, urlparse

    import redis

    # Parse the Redis URL
    url = urlparse(connection_str)

    # Connect to the Redis server
    db_name = str(url.path).strip("/")
    if db_name == "":
        db_name = "0"
    redis_client = redis.Redis(
        host=str(url.hostname),
        port=6379 if url.port is None else int(url.port),
        db=int(db_name),
        username=url.username,
        password=url.password,
    )

    # Store the result in Redis
    query_params = parse_qs(url.query)
    model_name = query_params.get("model", [""])[0]
    if model_name == "":
        raise Exception('"model" in Redic connection arguments is not provided.')

    return redis_client, model_name


def make_color(color, alpha=1):
    rgb = plt.matplotlib.colors.to_rgb(color)
    return f"rgba({rgb[0]*255}, {rgb[1]*255}, {rgb[2]*255}, {alpha})"


def update_graph(n, model_name):
    # Reset initial figure
    if n == 0:
        figure.last = dash.no_update

    # Acquire the lock at the beginning of the callback
    if not figure.lock.acquire(blocking=False):
        # If the lock is already acquired, skip this execution
        return figure.last

    try:
        start = datetime.now().timestamp()

        monitor: Optional[ModelMonitor] = figure.datasource(model_name)
        if monitor is None:
            figure.last = {
                "data": [],
                "layout": go.Layout(
                    title=f"Live data update of {model_name} is unavailable: model not monitored",
                    xaxis=dict(range=[0, scale], title="input_tokens(log2)"),
                    yaxis=dict(range=[0, scale], title="output_tokens(log2)"),
                ),
            }
            return figure.last

        if figure.debug_run:
            # Drive the monitor progress for debugging
            if figure.debug_driver is None:
                figure.debug_driver = monitor._run_yieldable(
                    True, window_scaling=interval_scaling
                )

            next(figure.debug_driver)

        data_df = monitor.dataframe
        if data_df is None or len(data_df) == 0:
            figure.last = {
                "data": [],
                "layout": go.Layout(
                    title=f"Live data update of {model_name} is unavailable: insufficient data",
                    xaxis=dict(range=[0, scale], title="input_tokens(log2)"),
                    yaxis=dict(range=[0, scale], title="output_tokens(log2)"),
                ),
            }
            return figure.last
        centers = monitor.centers
        labeled = monitor.labeled
        data_colors = [
            colors[int(label) % len(colors)] if label >= 0 else "black"
            for label in data_df["label"]
        ]
        # label_seen = len(centers)

        # recluster level 2
        # lvl2data.clear()
        # lvl2data.append(uncategorized)
        # clusterers[1].reset()
        # clusterers[1].insert(uncategorized)
        # lvl2labels, lvl2centers = clusterers[1].get_cluster_labels(lvl2data.xy)
        # labeled = reduce(lambda cnt, center: cnt+center.size, lvl2centers, labeled)
        # for i, label in enumerate(lvl2labels):
        #     if label < 0:
        #         continue
        #     lvl2data._color[i] = colors[(int(label)+label_seen) % len(colors)]
        # label_seen += len(lvl2centers)
        # if len(lvl2centers) > 0:
        #     center_df = pd.concat([center_df, pd.DataFrame(data=np.array([center.to_array(span) for center in lvl2centers]), columns=['x', 'y', 'radius', 'size'])], ignore_index=True)

        duration = (datetime.now().timestamp() - start) * 1000
        plotdata = [
            go.Scatter(
                x=data_df["input_tokens"],
                y=data_df["output_tokens"],
                mode="markers",
                name="major patterns",
                marker=dict(
                    color=data_colors,  # Specify the color of the marker
                    size=3,  # Set the size of the marker
                ),
            ),
            # go.Scatter(
            #     x=lvl2data.x,
            #     y=lvl2data.y,
            #     mode='markers',
            #     name='minor patterns',
            #     marker=dict(
            #         symbol='square',
            #         color=lvl2data.color,  # Specify the color of the marker
            #         size=3,            # Set the size of the marker
            #     )
            # ),
        ]
        if len(centers) > 0:
            center_df = pd.DataFrame(
                data=np.array([center.to_array() for center in centers]),
                columns=["x", "y", "radius", "size"],
            )
            # assign color to center_df
            center_colors = [
                make_color(colors[int(idx) % len(colors)], alpha=0.5)
                for idx in center_df.index
            ]
            # print(center_df['size'])
            plotdata.append(
                go.Scatter(
                    x=center_df["x"],
                    y=center_df["y"],
                    mode="markers",
                    name="RPS",
                    marker=dict(
                        sizeref=1,  # Adjust this value to control size
                        sizemode="diameter",
                        size=np.maximum(
                            (canvas_size / (scale + 2))
                            * (np.log2(center_df["size"]) + 1),
                            10,
                        ),  # Assuming you have a column with size values
                        color=center_colors,
                        symbol="circle",
                    ),
                )
            )

        figure.last = {
            "data": plotdata,
            "layout": go.Layout(
                title=f"Live Data Update({n}:{round(duration)}ms) of {model_name}, labeled: {round(labeled/len(data_df)*100, 2)}%, processed: {monitor.progress}",
                # xaxis=dict(range=[0, max(data['x']) + 1]),
                # yaxis=dict(range=[0, max(data['y']) + 1])
                xaxis=dict(range=[0, scale], title="input_tokens(log2)"),
                yaxis=dict(range=[0, scale], title="output_tokens(log2)"),
            ),
        }
        return figure.last
    except Exception as e:
        logger.error(f"Failed to prepare figure: {e}")
        import traceback

        traceback.print_exc()
    finally:
        # Release the lock at the end of the callback
        figure.lock.release()


def store_model_name(pathname):
    # Extract model_name from pathname (e.g., /dash/model_name/)
    try:
        model_name = pathname.strip("/").split("/")[-1]
    except IndexError:
        model_name = None  # Handle cases where model_name is not present
    return model_name


def init(prefix=""):
    app = dash.Dash(__name__, requests_pathname_prefix=prefix + "/")

    app.layout = html.Div(
        [
            dcc.Location(id="url", refresh=False),  # To access the URL
            dcc.Input(id="model-name-input", type="hidden", value=""),
            html.Div(id="model-info"),
            dcc.Interval(
                id="interval-component",
                interval=1000,  # in milliseconds
                n_intervals=0,  # start at 0
            ),
            dcc.Graph(
                id="live-graph",
                style={"width": f"{canvas_size}px", "height": f"{canvas_size}px"},
            ),
        ]
    )

    app.callback(Output("model-name-input", "value"), Input("url", "pathname"))(
        store_model_name
    )
    app.callback(
        Output("live-graph", "figure"),
        [
            Input("interval-component", "n_intervals"),
            Input("model-name-input", "value"),
        ],
    )(update_graph)

    return app


def mount_to(
    routes: List, prefix: str, datasrc: Callable[[str], Optional[ModelMonitor]]
):
    figure.datasource = datasrc
    figure.debug_run = False
    routes.append(Mount(prefix, WSGIMiddleware(init(prefix).server)))
    return routes


if __name__ == "__main__":
    logging.basicConfig(level=logging.DEBUG)
    logging.getLogger("pulp.apis.core").setLevel(logging.INFO)  # Suppress pulp logs

    import argparse

    parser = argparse.ArgumentParser(description="Please provide dataset path:")
    parser.add_argument("--dataset", type=str, default=None, help="Dataset path.")
    parser.add_argument("--scaledata", type=float, default=1, help="Dataset path.")
    parser.add_argument("--profile", type=str, default=None, help="Profile path.")
    parser.add_argument(
        "--redisprofile",
        type=str,
        default=None,
        help="Redis connection string for profiles.",
    )
    args = parser.parse_args()
    if args.dataset is not None:
        figure.datasource = lambda _: get_debug_model_montior(
            args.dataset,
            args.scaledata,
            profile=args.profile,
            redisprofile=args.redisprofile,
        )
    init().run_server(debug=True)
