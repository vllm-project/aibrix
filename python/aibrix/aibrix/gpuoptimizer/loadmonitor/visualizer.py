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

import dash
from dash import dcc, html
from dash.dependencies import Input, Output
from starlette.middleware.wsgi import WSGIMiddleware
from starlette.routing import Mount
import plotly.graph_objs as go
import random
import pandas as pd
from datetime import datetime
import threading
import numpy as np
from typing import List, Tuple, Union, Callable, Optional
from .loadreader import LoadReader, DatasetLoadReader
from .clusterer import Clusterer, DBSCANClusterer, MovingDBSCANClusterer
from functools import reduce
from . import helpers
from .helpers import DataPoint
import os
from .monitor import ModelMonitor

canvas_size = 1000
scale = 16

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

colors = ['red', 'green', 'pink', 'blue', 'navy', 'orange', 'purple', 'cyan', 'magenta', 'yellow', 'black', 'gray', 'brown', 'olive', 'teal', 'maroon']
debug_model_monitor = ModelMonitor("sharegpt", "0")
debug_run = None
datasource: Callable[[str], Optional[ModelMonitor]] = lambda model_name: debug_model_monitor
last_figure = dash.no_update
lock = threading.Lock()

def update_graph(n, model_name):
    global last_figure

    # Reset initial figure
    if n == 0:
        last_figure = dash.no_update

    # Acquire the lock at the beginning of the callback
    if not lock.acquire(blocking=False):
        # If the lock is already acquired, skip this execution
        return last_figure
    
    try:
        start = datetime.now().timestamp()

        monitor = datasource(model_name)
        if monitor == None:
            last_figure = {
                'data': [],
                'layout': go.Layout(
                    title=f'Live data update of {model_name} is unavailable: model not monitored',
                    xaxis=dict(range=[0, scale], title='input_tokens(log2)'),
                    yaxis=dict(range=[0, scale], title='output_tokens(log2)'),
                )
            }
            return last_figure

        # if monitor == debug_model_monitor:
        #     global debug_run
        #     # Drive the monitor progress for debugging
        #     if debug_run == None:
        #         debug_run = monitor._run_yieladble(True)
        #     next(debug_run)

        data_df = monitor.dataframe
        if data_df is None or len(data_df) == 0:
            last_figure = {
                'data': [],
                'layout': go.Layout(
                    title=f'Live data update of {model_name} is unavailable: insufficient data',
                    xaxis=dict(range=[0, scale], title='input_tokens(log2)'),
                    yaxis=dict(range=[0, scale], title='output_tokens(log2)'),
                )
            }
            return last_figure
        centers = monitor.centers
        labeled = monitor.labeled
        data_colors = [colors[int(label) % len(colors)] if label >= 0 else 'black' for label in data_df['label']]
        label_seen = len(centers)

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
                x=data_df['input_tokens'],
                y=data_df['output_tokens'],
                mode='markers',
                name='major patterns',
                marker=dict(
                    color=data_colors,  # Specify the color of the marker
                    size=3,            # Set the size of the marker
                )
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
            center_df = pd.DataFrame(data=np.array([center.to_array() for center in centers]), columns=['x', 'y', 'radius', 'size'])
             # assign color to center_df
            center_colors = [helpers.make_color(colors[int(idx) % len(colors)], alpha=0.5) 
                                  for idx in center_df.index]
            # print(center_df['size'])
            plotdata.append(
                go.Scatter(
                    x=center_df['x'],
                    y=center_df['y'],
                    mode='markers',
                    name='RPS',
                    marker=dict(
                        sizeref=1,  # Adjust this value to control size
                        sizemode='diameter', 
                        size=(canvas_size/(scale+2)) * np.log2(center_df['size']),  # Assuming you have a column with size values
                        color=center_colors, 
                        symbol='circle' 
                    )
                )
            )
        last_figure = {
            'data': plotdata,
            'layout': go.Layout(
                title=f'Live Data Update({n}:{round(duration)}ms) of {model_name}, labeled: {round(labeled/len(data_df)*100, 2)}%, processed: {monitor.progress}%',
                # xaxis=dict(range=[0, max(data['x']) + 1]),
                # yaxis=dict(range=[0, max(data['y']) + 1])
                xaxis=dict(range=[0, scale], title='input_tokens(log2)'),
                yaxis=dict(range=[0, scale], title='output_tokens(log2)'),
            )
        }
        return last_figure
    finally:
        # Release the lock at the end of the callback
        lock.release()

def store_model_name(pathname):
    # Extract model_name from pathname (e.g., /dash/model_name/)
    try:
        model_name = pathname.strip("/").split("/")[-1]  
    except IndexError:
        model_name = None  # Handle cases where model_name is not present
    return model_name

def init(prefix=""):
    app = dash.Dash(__name__, requests_pathname_prefix=prefix + '/')

    app.layout = html.Div([
        dcc.Location(id='url', refresh=False),  # To access the URL
        dcc.Input(id="model-name-input", type="hidden", value=""),
        html.Div(id="model-info"),
        dcc.Interval(id='interval-component',
            interval=100,  # in milliseconds
            n_intervals=0,  # start at 0
        ),
        dcc.Graph(id='live-graph',
            style={'width': f'{canvas_size}px', 'height': f'{canvas_size}px'}
        ),
    ])

    app.callback(Output("model-name-input", "value"), Input("url", "pathname"))(store_model_name)
    app.callback(Output('live-graph', 'figure'), [
        Input('interval-component', 'n_intervals'),
        Input("model-name-input", "value"),
    ])(update_graph)

    return app

def mount_to(routes: List, prefix: str, datasrc: Callable[[str], Optional[ModelMonitor]]):
    global datasource
    datasource = datasrc
    routes.append(Mount(prefix, WSGIMiddleware(init(prefix).server)))
    return routes

if __name__ == '__main__':
    init().run_server(debug=True)