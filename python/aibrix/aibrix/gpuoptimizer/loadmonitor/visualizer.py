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
import plotly.graph_objs as go
import random
import pandas as pd
from datetime import datetime
import threading
import numpy as np
from typing import List, Tuple, Union
from loadreader import LoadReader, DatasetLoadReader
from clusterer import Clusterer, DBSCANClusterer, MovingDBSCANClusterer
from functools import reduce
import helpers
from helpers import DataPoint

app = dash.Dash(__name__)

canvas_size = 1000
scale = 16

app.layout = html.Div([
    dcc.Graph(
        id='live-graph',
        style={'width': f'{canvas_size}px', 'height': f'{canvas_size}px'}
    ),
    dcc.Interval(
        id='interval-component',
        interval=100,  # in milliseconds
        n_intervals=0,
    )
])     

class DataBuffer:
    def __init__(self, cap):
        self._xy = np.empty((cap, 2), dtype=float)
        self._age = np.empty(cap, dtype=int)
        self._color = ["black"] * cap
        self._size = 0

    def append(self, tokens: List[DataPoint]):
        self._xy[self._size: self._size+len(tokens)] = tokens
        self._age[self._size: self._size+len(tokens)] = [token.age for token in tokens]
        self._size += len(tokens)

    def append_one(self, token: List[DataPoint]):
        self._xy[self._size] = token
        self._age[self._size] = token.age
        self._size += 1

    def trim_head(self, start):
        self._xy[:-start] = self._xy[start:]
        if start > 0:
            self._size -= start
        else:
            self._size = -start
    
    def clear(self):
        self._size = 0

    @property
    def x(self):
        return self._xy[:self._size, 0]
    
    @property
    def y(self):
        return self._xy[:self._size, 1]
    
    @property
    def xy(self):
        return self._xy[:self._size]
    
    def datapoint(self, i):
        return DataPoint(self._xy[i], self._age[i])
    
    @property
    def color(self):
        return self._color[:self._size]
    
    @property
    def len(self):
        return self._size
    
    @property
    def cap(self):
        return self._xy.size

window = 4000
# Load dataset reader
reader: LoadReader = DatasetLoadReader('data/sharegpt.csv', n_batch=100)
span = window / reader.n_batch
# Define clusterer
clusterers: List[Clusterer] = [MovingDBSCANClusterer(0.8, 100, 4, window), DBSCANClusterer(0.5, 10)]
# Simulated data source for display
data = DataBuffer(window)
lvl2data = DataBuffer(window)

colors = ['red', 'green', 'pink', 'blue', 'navy', 'orange', 'purple', 'cyan', 'magenta', 'yellow', 
          'black', 'gray', 'brown', 'olive', 'teal', 'maroon']
last_figure = dash.no_update
lock = threading.Lock()
@app.callback(Output('live-graph', 'figure'),
              Input('interval-component', 'n_intervals'))
def update_graph(n):
    global last_figure

    # Acquire the lock at the beginning of the callback
    if not lock.acquire(blocking=False):
        # If the lock is already acquired, skip this execution
        return last_figure
    
    try:
        start = datetime.now().timestamp()

        # window control
        movingCluster: MovingDBSCANClusterer = clusterers[0]
        if movingCluster.validate():
            # Data refreshing 
            data.trim_head(-movingCluster.length)
        
        tokens = [DataPoint([record.input_tokens, record.output_tokens],n) for record in reader.read()]  # read data
        movingCluster.insert(tokens)
        data.append(tokens)

        center_df = pd.DataFrame(columns=['x', 'y', 'radius', 'size'])
        labels, centers = movingCluster.get_cluster_labels(data.xy)
        labeled = reduce(lambda cnt, center: cnt+center.size, centers, 0)
        uncategorized = []
        for i, label in enumerate(labels):
            if label < 0:
                data._color[i] = 'black'
                uncategorized.append(data.datapoint(i))
                continue
            data._color[i] = colors[int(label) % len(colors)]
        label_seen = len(centers)
        if len(centers) > 0:
            center_df = pd.DataFrame(data=np.array([center.to_array(span) for center in centers]), columns=['x', 'y', 'radius', 'size'])

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

        # assign color to center_df
        center_df['color'] = [helpers.make_color(colors[int(idx) % len(colors)], alpha=0.5) for idx in center_df.index]
        print(center_df['size'])

        duration = (datetime.now().timestamp() - start) * 1000
        last_figure = {
            'data': [
                go.Scatter(
                    x=data.x,
                    y=data.y,
                    mode='markers',
                    name='major patterns',
                    marker=dict(
                        color=data.color,  # Specify the color of the marker
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
                go.Scatter(
                    x=center_df['x'],
                    y=center_df['y'],
                    mode='markers',
                    name='RPS',
                    marker=dict(
                        sizeref=1,  # Adjust this value to control size
                        sizemode='diameter', 
                        size=(canvas_size/(scale+2)) * np.log2(center_df['size']),  # Assuming you have a column with size values
                        color=center_df['color'], 
                        symbol='circle' 
                    )
                )
            ],
            'layout': go.Layout(
                title=f'Live Data Update({n}:{round(duration)}ms), labeled: {round(labeled/len(labels)*100, 2)}%, processed: {round(reader.progress()*100, 2)}%',
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

if __name__ == '__main__':
    app.run_server(debug=True)