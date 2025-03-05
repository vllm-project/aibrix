from typing import List, Any, Dict
import numpy as np

from scipy import stats
from scipy.optimize import minimize

def generate_poisson_dist(target: int, sample_size: int, smooth_window_size: int = 1) -> List[int]:
    rps_list = np.random.poisson(lam=target, size=sample_size).tolist()
    # Apply moving average smoothing
    smoothed = []
    for i in range(len(rps_list)):
        start = max(0, i - smooth_window_size + 1)
        window = rps_list[start:i + 1]
        smoothed.append(max(1, round(sum(window) / len(window))))
    return smoothed

def generate_token_len_from_percentiles(
    median: int,
    percentiles: List[float],
    token_lengths: List[int],
    period: int,
    total_seconds: int,
    scale: float,
    amplitude_factor: float = 0.2  # Controls the amplitude of sinusoidal variation
) -> List[int]:
    for idx in range(len(token_lengths) - 1):
        if token_lengths[idx] >= token_lengths[idx + 1]:
            raise ValueError("Percentiles must be strictly increasing")
    if not len(percentiles) == len(token_lengths):
        raise ValueError(f"percentiles and token_lengths should have matching length: {percentiles} : {token_lengths}")
    if token_lengths[0] <= 0:
        raise ValueError("Token lengths must be positive")
    
    token_lengths = [x / scale for x in token_lengths]
    log_lengths = np.log(token_lengths)
    def objective(params, percs, lengths):
        mu, sigma = params
        expected = stats.norm.ppf(percs, mu, sigma)
        return np.sum((expected - lengths) ** 2)
    result = minimize(
        objective,
        x0=[np.mean(log_lengths), np.std(log_lengths)],
        args=(percentiles, log_lengths),
        method='Nelder-Mead'
    )
    mu, sigma = result.x
    t = np.arange(total_seconds)
    amplitude = median * amplitude_factor
    sinusoidal_variation = amplitude * np.sin(2 * np.pi * t / period)
    base_samples = np.random.lognormal(mu, sigma, size=total_seconds)
    scale_factor = median / np.median(base_samples)
    token_len_list = base_samples * scale_factor + sinusoidal_variation
    token_len_list = [int(max(1, x)) for x in token_len_list]
    return token_len_list

def to_fluctuate_pattern_config(config_type: str,
                                mean: float,
                            ) -> Dict[str, Any]:
    if config_type == 'quick_rising':
        return {'A': 0.5 * mean, 
                'B': mean, 
                'sigma': 0.1,
                'period': 5,
                'omega': None,
                'only_rise': True}
    elif config_type == 'slow_rising':
        return {'A': 0.5 * mean, 
                'B': mean, 
                'sigma': 0.1,
                'period': 0.25,
                'omega': None,
                'only_rise': True}
    elif config_type == 'slight_fluctuation':
        return {'A': 0.1 * mean, 
                'B': mean, 
                'sigma': 0.1,
                'period': 1, 
                'omega': None,
                'only_rise': False}
    elif config_type == 'severe_fluctuation':
        return {'A': 0.5 * mean, 
                'B': mean,
                'sigma': 0.1,
                'period': 12, 
                'omega': None,
                'only_rise': False}
    else:
        raise ValueError(f"Unknown config type: {config_type}")
    
    
def user_to_synthetic_config(user_config: Dict,
                             duration_ms: int,):
    return {
        'A': float(user_config['fluctuate']), 
        'B': float(user_config['mean']),
        'sigma': float(user_config['noise']),
        'period': duration_ms/float(user_config['period_len_ms']), 
        'omega': None,
        'only_rise': user_config['only_rise'],
    }